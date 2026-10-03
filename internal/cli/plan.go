package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/compose"
	"github.com/danthegoodman1/simplecloud/internal/plan"
)

func planCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Show what deploying would do, without changing anything",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, done, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			cp, proj, err := e.loadProject(true)
			if err != nil {
				return err
			}
			pl, err := e.buildPlan(cp, proj)
			if err != nil {
				return err
			}
			if flags.JSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(pl)
			}
			renderPlan(cmd.OutOrStdout(), cp, pl)
			return nil
		},
	}
}

// renderPlan writes a deterministic description of the end state. Nothing here
// varies between runs, so two plans of an unchanged project are identical.
func renderPlan(w io.Writer, cp *compose.Project, pl *plan.Plan) {
	fmt.Fprintf(w, "Project   %s  (%s)\n", pl.Project.Name, pl.Project.ID)
	fmt.Fprintf(w, "Directory %s\n", pl.Project.Dir)
	fmt.Fprintf(w, "Network   %s   hub %s on %s:%d\n\n",
		pl.Project.OverlayCIDR, pl.Hub.OverlayIP, endpointOrUnset(pl.Hub.Endpoint), pl.Hub.ListenPort)

	for _, sp := range pl.Services {
		r := sp.Resolved
		fmt.Fprintf(w, "service %s  [%s]\n", sp.Name, sp.Action)
		if r.Image != "" {
			fmt.Fprintf(w, "  image      %s\n", r.Image)
			if r.ImageDigest != "" {
				fmt.Fprintf(w, "  digest     %s\n", r.ImageDigest)
			}
		} else if b := sp.Resolved.Service.Build; b != nil {
			fmt.Fprintf(w, "  build      context %s, dockerfile %s\n", b.Context, b.Dockerfile)
		}
		if len(r.Command) > 0 {
			fmt.Fprintf(w, "  command    %s\n", strings.Join(r.Command, " "))
		}
		fmt.Fprintf(w, "  resources  %d vCPU, %d MiB\n", r.Service.VCPU, r.Service.MemMiB)
		fmt.Fprintf(w, "  sleep      %s\n", sleepDescription(r.Service))
		if len(r.ReachablePorts) > 0 {
			fmt.Fprintf(w, "  reachable  %s\n", joinInts(r.ReachablePorts))
		}
		if len(r.PublishPorts) > 0 {
			fmt.Fprintf(w, "  publish    %s\n", joinInts(r.PublishPorts))
		}
		for _, sk := range r.SkippedPorts {
			fmt.Fprintf(w, "  not published  %d — %s port; reachable as %s:%d inside the project\n",
				sk.Container, sk.Reason, sp.Name, sk.Container)
		}
		if n := len(r.Env); n > 0 {
			fmt.Fprintf(w, "  env        %d variable(s)\n", n)
		}
		for _, sl := range sp.Slots {
			fmt.Fprintf(w, "  slot %-14s overlay %-13s relay %-12s doorbell %d\n",
				sl.Name, sl.OverlayIP, sl.LoopbackIP, sl.Doorbell)
		}
		fmt.Fprintln(w)
	}

	if len(pl.Volumes) > 0 {
		fmt.Fprintln(w, "volumes")
		for _, v := range pl.Volumes {
			id := v.DiskID
			if id == "" {
				id = "(to create)"
			}
			fmt.Fprintf(w, "  %-22s %-6s %-28s at %s  owner %s\n", v.Name, v.Kind, id, v.MountPath, v.OwnerSlot)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "hub %s  listen %d\n", pl.Hub.Interface, pl.Hub.ListenPort)
	for _, peer := range pl.Hub.Peers {
		fmt.Fprintf(w, "  peer %-14s %s  allowed %s\n", peer.Slot, peer.PublicKey, peer.AllowedIP)
	}
}

func endpointOrUnset(v string) string {
	if v == "" {
		return "<hub not configured>"
	}
	return v
}

func sleepDescription(s *compose.Service) string {
	switch {
	case s.KeepAwake && !s.KeepAwakeExplicit():
		return "never (no reachable port, so activity cannot be observed)"
	case s.KeepAwake:
		return "never (keep-awake)"
	case s.IdleTimeout == 0:
		return "never (idle timeout disabled)"
	default:
		return "after " + s.IdleTimeout.String() + " idle"
	}
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ", ")
}
