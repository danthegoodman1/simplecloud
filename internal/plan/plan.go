package plan

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/danthegoodman1/simplecloud/internal/compose"
	"github.com/danthegoodman1/simplecloud/internal/state"
	"github.com/danthegoodman1/simplecloud/internal/wg"
)

// Plan is the end state to converge on. Building one performs no remote work
// beyond reading image configuration, so it is safe to print and diff.
type Plan struct {
	Project  *state.Project
	Hub      HubPlan
	Services []*ServicePlan
	Volumes  []*VolumePlan
}

type HubPlan struct {
	Interface  string
	ListenPort int
	Endpoint   string
	OverlayIP  string
	Peers      []PeerPlan
}

type PeerPlan struct {
	Slot      string
	PublicKey string
	AllowedIP string
}

type ServicePlan struct {
	Name     string
	Resolved *Resolved
	Slots    []*SlotPlan
	// Action says what converging will do, and is what makes an unchanged project
	// a no-op rather than a redeploy.
	Action string
}

type SlotPlan struct {
	Slot       *state.Slot
	Name       string
	OverlayIP  string
	LoopbackIP string
	Publish    []int
	Doorbell   int
}

type VolumePlan struct {
	Name      string
	Kind      string
	DiskID    string
	MountPath string
	OwnerSlot string
	Action    string
}

// Build resolves a Compose project into a plan, allocating any identity that does
// not exist yet. Allocation is persisted, so a second Build returns the same
// addresses and keys and its output is byte-identical.
func Build(ctx context.Context, st *state.Store, f *Fetcher, p *compose.Project, proj *state.Project, endpoint string, profiles []string) (*Plan, error) {
	pl := &Plan{
		Project: proj,
		Hub: HubPlan{
			Interface:  proj.InterfaceName(),
			ListenPort: proj.HubListenPort,
			Endpoint:   endpoint,
			OverlayIP:  proj.HubOverlayIP(),
		},
	}

	volumeOwner := map[string]string{}
	for _, svc := range p.ActiveServices(profiles) {
		imageRef := svc.Image
		if imageRef == "" {
			// A built service has no reference until the build has happened, so the
			// plan names what it will become rather than resolving it now.
			imageRef = ""
		}
		sp := &ServicePlan{Name: svc.Name}
		if imageRef == "" {
			sp.Action = "build then create"
			sp.Resolved = &Resolved{Service: svc, Env: map[string]string{}}
		} else {
			res, err := Resolve(ctx, f, p.Dir, svc, imageRef)
			if err != nil {
				return nil, err
			}
			sp.Resolved = res
			existing, err := st.GetService(proj.ID, svc.Name)
			if err != nil {
				return nil, err
			}
			switch {
			case existing == nil:
				sp.Action = "create"
			case existing.ConfigHash != res.ConfigHash:
				sp.Action = "replace (configuration changed)"
			default:
				sp.Action = "unchanged"
			}
		}

		for ordinal := 1; ordinal <= svc.Replicas; ordinal++ {
			slot, err := ensureSlot(st, proj, svc, ordinal)
			if err != nil {
				return nil, err
			}
			sp.Slots = append(sp.Slots, &SlotPlan{
				Slot: slot, Name: slot.Name, OverlayIP: slot.OverlayIP,
				LoopbackIP: slot.LoopbackIP, Publish: sp.Resolved.PublishPorts,
				Doorbell: svc.DoorbellPort,
			})
			pl.Hub.Peers = append(pl.Hub.Peers, PeerPlan{
				Slot: slot.Name, PublicKey: slot.WGPublicKey, AllowedIP: slot.OverlayIP + "/32",
			})
		}

		pl.Services = append(pl.Services, sp)

		// A volume belongs to slot 1 of the service that declares it, and is never
		// shared between replicas.
		for _, m := range svc.Mounts {
			if owner, taken := volumeOwner[m.Volume]; taken {
				return nil, fmt.Errorf("volume %q is mounted by both %s and %s; a volume has one writer",
					m.Volume, owner, svc.Name)
			}
			volumeOwner[m.Volume] = svc.Name
		}
	}

	sort.Slice(pl.Hub.Peers, func(i, j int) bool { return pl.Hub.Peers[i].Slot < pl.Hub.Peers[j].Slot })

	existingVolumes := map[string]*state.Volume{}
	vols, err := st.ListVolumes(proj.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		existingVolumes[v.Name] = v
	}
	for _, svc := range p.ActiveServices(profiles) {
		for _, m := range svc.Mounts {
			vp := &VolumePlan{Name: m.Volume, Kind: "volume", MountPath: m.Path, OwnerSlot: svc.Name + "-1"}
			if ex, ok := existingVolumes[m.Volume]; ok {
				vp.DiskID, vp.Action = ex.DiskID, "existing"
			} else {
				vp.Action = "create"
			}
			pl.Volumes = append(pl.Volumes, vp)
		}
		for _, sy := range svc.Sync {
			nm := syncVolumeName(svc.Name, sy.Path)
			vp := &VolumePlan{Name: nm, Kind: "sync", MountPath: sy.Path, OwnerSlot: svc.Name + "-1", Action: "create"}
			if ex, ok := existingVolumes[nm]; ok {
				vp.DiskID, vp.Action = ex.DiskID, "existing"
			}
			pl.Volumes = append(pl.Volumes, vp)
		}
	}
	sort.Slice(pl.Volumes, func(i, j int) bool { return pl.Volumes[i].Name < pl.Volumes[j].Name })
	return pl, nil
}

// syncVolumeName names a synced path's disk after its service and mount point, so
// a project's disks group together and stay recognizable.
func syncVolumeName(service, path string) string {
	clean := strings.Trim(strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "-"), "-")
	if clean == "" {
		clean = "root"
	}
	return "sync-" + service + "-" + clean
}

func ensureSlot(st *state.Store, proj *state.Project, svc *compose.Service, ordinal int) (*state.Slot, error) {
	name := fmt.Sprintf("%s-%d", svc.Name, ordinal)
	if existing, err := st.GetSlot(proj.ID, name); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	priv, err := wg.GenerateKey()
	if err != nil {
		return nil, err
	}
	return st.AllocateSlot(proj, svc.Name, ordinal, priv.String(), priv.Public().String(), svc.DoorbellPort)
}

// DiskName is the account-wide name for a project's disk, project first so a
// project's disks sort together.
func DiskName(projectName, volume string) string {
	return projectName + "-" + volume
}
