// Package deploy converges a project onto the state a plan describes.
//
// Every step is idempotent and re-runnable: a command that cannot be interrupted
// and retried is a defect, because an interrupted deploy is the normal case.
package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danthegoodman1/simplecloud/internal/agent"
	"github.com/danthegoodman1/simplecloud/internal/agentbin"
	"github.com/danthegoodman1/simplecloud/internal/archil"
	"github.com/danthegoodman1/simplecloud/internal/compose"
	"github.com/danthegoodman1/simplecloud/internal/hub"
	"github.com/danthegoodman1/simplecloud/internal/plan"
	"github.com/danthegoodman1/simplecloud/internal/state"
)

type Deployer struct {
	Ctx      context.Context
	Store    *state.Store
	Client   *archil.Client
	Hub      *hub.Client
	HubRec   *state.Hub
	Fetcher  *plan.Fetcher
	Registry *archil.RegistryAuth
	Out      io.Writer
	// ForceAgent reinstalls the agent even when a slot is answering, for iterating
	// on the agent itself.
	ForceAgent bool
}

// agentReachable reports whether a slot's agent answers, without waking it: a
// paused slot is reported unreachable rather than resumed to find out.
func (d *Deployer) agentReachable(slot *state.Slot) bool {
	if slot.SandboxID == "" || slot.DoorbellHost == "" {
		return false
	}
	sb, err := d.Client.GetSandbox(d.Ctx, slot.SandboxID)
	if err != nil || sb.Status != archil.StatusRunning {
		// A sleeping slot is working as intended; its agent is restored on wake.
		return sb != nil && sb.Status == archil.StatusPaused
	}
	_, err = d.AgentStatus(slot)
	return err == nil
}

// step prints progress with its elapsed time. A deploy spans tens of seconds, and
// silence is indistinguishable from a hang.
func (d *Deployer) step(format string, args ...any) func(string, ...any) {
	start := time.Now()
	fmt.Fprintf(d.Out, "  %-46s", fmt.Sprintf(format, args...))
	return func(result string, rargs ...any) {
		msg := fmt.Sprintf(result, rargs...)
		fmt.Fprintf(d.Out, "%s (%s)\n", msg, time.Since(start).Round(100*time.Millisecond))
	}
}

func (d *Deployer) note(format string, args ...any) {
	fmt.Fprintf(d.Out, "  %s\n", fmt.Sprintf(format, args...))
}

// Up converges the project. Naming services restricts the work to those, which is
// how one service is updated without disturbing the rest of the stack.
func (d *Deployer) Up(cp *compose.Project, proj *state.Project, pl *plan.Plan, only []string) error {
	wanted := map[string]bool{}
	for _, n := range only {
		wanted[n] = true
	}

	fmt.Fprintf(d.Out, "project %s  (%s)\n", proj.Name, proj.OverlayCIDR)

	// The hub first: slot keys and addresses come from the plan, so peers can be
	// configured before any sandbox exists.
	if err := d.converge_hub(proj, pl); err != nil {
		return err
	}
	if err := d.ensureDisks(proj, pl); err != nil {
		return err
	}

	// Pass one creates sandboxes and their doorbells, because an agent's config
	// needs every peer's doorbell before it can wake anything.
	type pending struct {
		svc  *compose.Service
		sp   *plan.ServicePlan
		slot *state.Slot
	}
	var toStart []pending
	for _, sp := range pl.Services {
		svc := sp.Resolved.Service
		if len(wanted) > 0 && !wanted[svc.Name] {
			continue
		}
		for _, slp := range sp.Slots {
			slot, created, err := d.ensureSandbox(proj, sp, slp)
			if err != nil {
				return fmt.Errorf("%s: %w", slp.Name, err)
			}
			if created {
				toStart = append(toStart, pending{svc: svc, sp: sp, slot: slot})
				continue
			}
			// An unchanged service still needs its agent repaired if the agent is
			// not answering: a cold boot loses every process, and a CLI upgrade
			// carries a newer agent. So `up` is also the repair path, rather than
			// something a separate command has to notice.
			if d.ForceAgent || !d.agentReachable(slot) {
				why := "not answering"
				if d.ForceAgent {
					why = "forced"
				}
				d.note("%s: reinstalling the agent (%s)", slot.Name, why)
				toStart = append(toStart, pending{svc: svc, sp: sp, slot: slot})
			}
		}
		if err := d.recordService(proj, sp); err != nil {
			return err
		}
	}

	if len(toStart) == 0 {
		d.note("nothing to do; every service matches its configuration")
		return nil
	}

	// Pass two installs the agent with the full peer map and starts it.
	for _, p := range toStart {
		if err := d.startAgent(cp, proj, pl, p.svc, p.slot); err != nil {
			return fmt.Errorf("%s: %w", p.slot.Name, err)
		}
	}
	// Readiness last, in dependency order, so a dependent is only called ready once
	// what it depends on is reachable.
	for _, p := range orderByDependency(cp, toStart, func(p pending) string { return p.svc.Name }) {
		if err := d.waitReady(p.slot, 3*time.Minute); err != nil {
			return fmt.Errorf("%s: %w", p.slot.Name, err)
		}
	}
	return nil
}

func (d *Deployer) converge_hub(proj *state.Project, pl *plan.Plan) error {
	doneStep := d.step("hub interface %s", pl.Hub.Interface)
	pub, err := d.Hub.EnsureInterface(d.Ctx, pl.Hub.Interface, pl.Hub.ListenPort, pl.Hub.OverlayIP)
	if err != nil {
		return err
	}
	if pub != proj.HubPublicKey {
		if err := d.Store.SetHubPublicKey(proj.ID, pub); err != nil {
			return err
		}
		proj.HubPublicKey = pub
	}
	doneStep("listening on %d", pl.Hub.ListenPort)

	peers := make([]hub.Peer, 0, len(pl.Hub.Peers))
	for _, p := range pl.Hub.Peers {
		peers = append(peers, hub.Peer{Name: p.Slot, PublicKey: p.PublicKey, AllowedIP: p.AllowedIP})
	}
	doneStep = d.step("hub peers")
	if err := d.Hub.SyncPeers(d.Ctx, pl.Hub.Interface, pl.Hub.ListenPort, peers); err != nil {
		return err
	}
	doneStep("%d peer(s)", len(peers))

	// Rebuild the whole table from every project. Full replacement is convergent,
	// and the same-interface rules are what keep a shared hub from bridging
	// projects: forwarding plus a route per project would otherwise do exactly that.
	projects, err := d.Store.ListProjects()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(projects))
	for _, p := range projects {
		names = append(names, p.InterfaceName())
	}
	sort.Strings(names)
	doneStep = d.step("hub firewall")
	if err := d.Hub.ApplyFirewall(d.Ctx, names); err != nil {
		return err
	}
	doneStep("%d project interface(s) isolated", len(names))
	return nil
}

func (d *Deployer) ensureDisks(proj *state.Project, pl *plan.Plan) error {
	for _, vp := range pl.Volumes {
		existing, err := d.Store.ListVolumes(proj.ID)
		if err != nil {
			return err
		}
		var found *state.Volume
		for _, v := range existing {
			if v.Name == vp.Name {
				found = v
				break
			}
		}
		if found != nil && found.DiskID != "" {
			continue
		}
		doneStep := d.step("disk for volume %s", vp.Name)
		res, err := d.Client.CreateDisk(d.Ctx, plan.DiskName(proj.Name, vp.Name))
		if err != nil {
			return err
		}
		if err := d.Store.PutVolume(&state.Volume{
			ProjectID: proj.ID, Name: vp.Name, DiskID: res.Disk.ID,
			MountToken: res.Token, MountPath: vp.MountPath,
			OwnerSlot: vp.OwnerSlot, Kind: vp.Kind,
		}); err != nil {
			return err
		}
		doneStep("%s", res.Disk.ID)
	}
	return nil
}

// ensureSandbox creates a sandbox for a slot when it needs one, and reports
// whether it did. An unchanged service with a live sandbox is left alone.
func (d *Deployer) ensureSandbox(proj *state.Project, sp *plan.ServicePlan, slp *plan.SlotPlan) (*state.Slot, bool, error) {
	slot := slp.Slot
	svc := sp.Resolved.Service

	if slot.SandboxID != "" {
		sb, err := d.Client.GetSandbox(d.Ctx, slot.SandboxID)
		if err == nil {
			recorded, err := d.Store.GetService(proj.ID, svc.Name)
			if err != nil {
				return nil, false, err
			}
			unchanged := recorded != nil && recorded.ConfigHash == sp.Resolved.ConfigHash
			if unchanged {
				switch sb.Status {
				case archil.StatusRunning, archil.StatusPaused:
					slot.SandboxStatus = string(sb.Status)
					return slot, false, d.Store.PutSlot(slot)

				case archil.StatusStopped, archil.StatusExited, archil.StatusFailed:
					// Preemption, host failure, or a pause that did not complete. The
					// disk survives, so start the sandbox rather than replacing it:
					// replacing would discard everything written outside a declared
					// volume, which a cold boot from disk keeps.
					doneStep := d.step("%s starting after %s", slot.Name, sb.Status)
					started, err := d.Client.StartSandbox(d.Ctx, sb.ID)
					if err != nil {
						return nil, false, fmt.Errorf("starting after %s: %w", sb.Status, err)
					}
					slot.SandboxStatus = string(started.Status)
					if err := d.Store.PutSlot(slot); err != nil {
						return nil, false, err
					}
					doneStep("%s, disk preserved", started.Status)
					// A cold boot runs no processes, so the agent has to be installed
					// and started again even though its files are still on the disk.
					return slot, true, nil
				}
			}
			// Configuration changed. A sandbox's environment is fixed at creation, so
			// the only way to apply it is a replacement.
			doneStep := d.step("%s replacing sandbox", slot.Name)
			if _, err := d.Client.StopSandbox(d.Ctx, sb.ID); err != nil {
				d.note("stopping %s: %v", sb.ID, err)
			}
			if err := d.Client.DeleteSandbox(d.Ctx, sb.ID); err != nil {
				d.note("deleting %s: %v", sb.ID, err)
			}
			doneStep("old sandbox removed")
			slot.SandboxID = ""
		} else {
			// The sandbox is gone from under us; converge rather than fail.
			slot.SandboxID = ""
		}
	}

	imageRef := svc.Image
	doneStep := d.step("%s resolving %s", slot.Name, imageRef)
	img, err := d.Client.ResolveImage(d.Ctx, imageRef, d.Registry)
	if err != nil {
		return nil, false, fmt.Errorf("resolving image %s: %w", imageRef, err)
	}
	doneStep("%s", shortDigest(img.Digest))

	zero := 0
	doneStep = d.step("%s creating sandbox", slot.Name)
	sb, err := d.Client.CreateSandbox(d.Ctx, archil.CreateSandboxRequest{
		Name:       sandboxName(proj.Name, slot.Name),
		ImageID:    img.ImageID,
		VCPUCount:  svc.VCPU,
		MemSizeMiB: svc.MemMiB,
		// Archil's own idle timeout counts attached process connections rather than
		// traffic, so it would pause a busy service. Sleeping is accounted for by
		// the agent instead.
		IdleTTLSeconds: &zero,
		MaxTTLSeconds:  svc.MaxTTL,
	}, true)
	if err != nil {
		return nil, false, err
	}
	doneStep("%s, %d vCPU, %d MiB", sb.Status, svc.VCPU, svc.MemMiB)

	slot.SandboxID = sb.ID
	slot.SandboxStatus = string(sb.Status)
	if err := d.Store.PutSlot(slot); err != nil {
		return nil, false, err
	}

	// Egress has to be enabled after creation: the create call accepts a network
	// field and silently ignores it.
	doneStep = d.step("%s enabling egress", slot.Name)
	network := archil.AllowAllEgress()
	if len(svc.Egress) > 0 {
		network = archil.AllowListEgress(svc.Egress)
	}
	if err := d.Client.UpdateNetwork(d.Ctx, sb.ID, network); err != nil {
		if ae, ok := err.(*archil.APIError); ok && ae.PlanRequired() {
			return nil, false, fmt.Errorf("egress requires a Team plan on this Archil account.\n" +
				"  Without it a sandbox cannot reach the hub, so no overlay can form")
		}
		return nil, false, err
	}
	if len(svc.Egress) > 0 {
		doneStep("restricted to %d host(s) plus Archil", len(svc.Egress))
	} else {
		doneStep("allowed")
	}

	if err := d.ensureDoorbell(slot, svc.DoorbellPort); err != nil {
		return nil, false, err
	}
	if err := d.exposePorts(proj, slot, sp.Resolved.PublishPorts); err != nil {
		return nil, false, err
	}
	return slot, true, nil
}

// ensureDoorbell mints the port token that makes a private service wakeable.
//
// The port is never published: an authenticated request wakes the sandbox, and an
// unauthenticated one is refused and leaves it paused, so a database needs no
// public exposure to be reachable on demand.
func (d *Deployer) ensureDoorbell(slot *state.Slot, port int) error {
	doneStep := d.step("%s doorbell", slot.Name)
	tok, err := d.Client.CreatePortToken(d.Ctx, slot.SandboxID, port, "24h")
	if err != nil {
		return fmt.Errorf("creating the doorbell token: %w", err)
	}
	slot.DoorbellPort = port
	slot.DoorbellHost = tok.Hostname
	slot.DoorbellToken = tok.Token
	slot.DoorbellTokenID = tok.ID
	slot.DoorbellExpires = tok.ExpiresAt.Unix()
	doneStep("token-only on port %d", port)
	return d.Store.PutSlot(slot)
}

func (d *Deployer) exposePorts(proj *state.Project, slot *state.Slot, ports []int) error {
	for _, port := range ports {
		doneStep := d.step("%s publishing %d", slot.Name, port)
		ep, err := d.Client.ExposePort(d.Ctx, slot.SandboxID, port)
		if err != nil {
			return err
		}
		if err := d.Store.PutExposure(&state.Exposure{
			ProjectID: proj.ID, Slot: slot.Name, Port: port, Hostname: ep.Hostname,
		}); err != nil {
			return err
		}
		doneStep("https://%s", ep.Hostname)
	}
	return nil
}

func sandboxName(project, slot string) string {
	name := strings.ToLower(project + "-" + slot)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func newToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("deploy: generating a token: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// orderByDependency returns items so that a service appears after everything it
// depends on. A cycle falls back to declaration order rather than refusing.
func orderByDependency[T any](cp *compose.Project, items []T, nameOf func(T) string) []T {
	index := map[string]T{}
	for _, it := range items {
		index[nameOf(it)] = it
	}
	var out []T
	seen := map[string]bool{}
	var visit func(string, map[string]bool)
	visit = func(name string, path map[string]bool) {
		if seen[name] || path[name] {
			return
		}
		path[name] = true
		if svc := cp.Service(name); svc != nil {
			for _, dep := range svc.DependsOn {
				visit(dep, path)
			}
		}
		delete(path, name)
		seen[name] = true
		if it, ok := index[name]; ok {
			out = append(out, it)
		}
	}
	for _, it := range items {
		visit(nameOf(it), map[string]bool{})
	}
	return out
}

var _ = bytes.NewReader
var _ = os.Getenv
var _ = filepath.Join
var _ = agent.ConfigPath
var _ = agentbin.Bytes
