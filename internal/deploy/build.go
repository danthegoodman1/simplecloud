package deploy

import (
	"fmt"
	"strings"

	"github.com/danthegoodman1/simplecloud/internal/build"
	"github.com/danthegoodman1/simplecloud/internal/compose"
	"github.com/danthegoodman1/simplecloud/internal/state"
)

// BuildAll builds every service with a build: section and rewrites its image to
// the pushed tag, so the rest of the deploy sees an ordinary image reference.
//
// A build is skipped when its content hash is unchanged. The hash is the tag, so
// the image for unchanged source already exists in the registry and a push would
// be a no-op anyway.
func (d *Deployer) BuildAll(cp *compose.Project, proj *state.Project, registry string, only []string) error {
	wanted := map[string]bool{}
	for _, n := range only {
		wanted[n] = true
	}
	var pending []*compose.Service
	for _, svc := range cp.ActiveServices(nil) {
		if svc.Build == nil {
			continue
		}
		if len(wanted) > 0 && !wanted[svc.Name] {
			continue
		}
		pending = append(pending, svc)
	}
	if len(pending) == 0 {
		return nil
	}
	if registry == "" {
		names := make([]string, len(pending))
		for i, s := range pending {
			names[i] = s.Name
		}
		return fmt.Errorf("%s use build:, but no registry is configured.\n"+
			"  Archil hosts no registry, so built images need somewhere to live.\n"+
			"  Set SIMPLECLOUD_REGISTRY to a registry you can push to, and run docker login",
			strings.Join(names, ", "))
	}

	for _, svc := range pending {
		spec, err := build.Resolve(cp.Dir, registry, proj.Name, svc)
		if err != nil {
			return err
		}
		recorded, err := d.Store.GetService(proj.ID, svc.Name)
		if err != nil {
			return err
		}
		if recorded != nil && recorded.BuildHash == spec.Hash && recorded.Image == spec.Tag {
			d.note("%s: unchanged since the last build (%s)", svc.Name, spec.Hash[:12])
			svc.Image = spec.Tag
			continue
		}
		doneStep := d.step("%s building and pushing", svc.Name)
		fmt.Fprintln(d.Out)
		if err := spec.Build(d.Out); err != nil {
			return err
		}
		doneStep("%s", spec.Tag)
		svc.Image = spec.Tag
		// Record the hash now, so an interrupted deploy does not rebuild what it
		// already pushed.
		if recorded == nil {
			recorded = &state.Service{ProjectID: proj.ID, Name: svc.Name}
		}
		recorded.BuildHash = spec.Hash
		recorded.Image = spec.Tag
		if err := d.Store.PutService(recorded); err != nil {
			return err
		}
	}
	return nil
}
