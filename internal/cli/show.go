package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/archil"
	"github.com/danthegoodman1/simplecloud/internal/state"
)

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <service|slot>",
		Short: "Show everything about one slot, including how to reach it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, done, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer done()
			_, proj, err := e.loadProject(false)
			if err != nil {
				return err
			}
			slots, err := e.selectSlots(proj, args)
			if err != nil {
				return err
			}
			slot := slots[0]
			svc, _ := e.store.GetService(proj.ID, slot.Service)
			out := cmd.OutOrStdout()

			status := "not created"
			if slot.SandboxID != "" {
				if sb, err := e.client.GetSandbox(e.ctx, slot.SandboxID); err == nil {
					status = displayState(string(sb.Status))
				} else {
					status = "missing"
				}
			}
			if flags.JSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"slot": slot, "service": svc, "state": status})
			}

			var spec struct {
				IdleSeconds int   `json:"idle_seconds"`
				KeepAwake   bool  `json:"keep_awake"`
				LogRing     int64 `json:"log_ring"`
				Replicas    int   `json:"replicas"`
			}
			if svc != nil {
				_ = json.Unmarshal([]byte(svc.Spec), &spec)
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			row := func(k, v string, args ...any) {
				fmt.Fprintf(tw, "%s\t%s\n", k, fmt.Sprintf(v, args...))
			}
			replicas := spec.Replicas
			if replicas == 0 {
				replicas = 1
			}
			row("Service", "%s (slot %d of %d)", slot.Service, slot.Ordinal, replicas)
			row("Project", "%s", proj.Name)
			row("State", "%s", sleepLine(status, spec.IdleSeconds, spec.KeepAwake))
			if slot.SandboxID != "" {
				row("Sandbox", "%s", slot.SandboxID)
			}
			if svc != nil && svc.Image != "" {
				digest := svc.ImageDigest
				if digest != "" {
					row("Image", "%s@%s", svc.Image, digest)
				} else {
					row("Image", "%s", svc.Image)
				}
			}
			names := []string{slot.Service, slot.Name}
			sort.Strings(names)
			row("Overlay", "%s   reachable as %s", slot.OverlayIP, strings.Join(names, ", "))
			row("Relay alias", "%s", slot.LoopbackIP)

			vols, _ := e.store.ListVolumes(proj.ID)
			for _, v := range vols {
				if v.OwnerSlot != slot.Name {
					continue
				}
				holder := "unknown"
				if ds, err := e.client.ListDelegations(e.ctx, v.DiskID); err == nil {
					if len(ds) == 0 {
						holder = "none"
					} else {
						holder = "held"
					}
				}
				row("Volume", "%s → %s   %s  delegation %s", v.Name, v.MountPath, v.DiskID, holder)
			}
			exposures, _ := e.store.ListExposures(proj.ID)
			for _, x := range exposures {
				if x.Slot == slot.Name {
					row("Published", "%d  https://%s", x.Port, x.Hostname)
				}
			}
			if slot.DoorbellHost != "" {
				expiry := "unknown"
				if slot.DoorbellExpires > 0 {
					expiry = shortDuration(time.Until(time.Unix(slot.DoorbellExpires, 0)))
				}
				row("Doorbell", "port %d, token expires in %s", slot.DoorbellPort, expiry)
			}
			row("Hub peer", "%s  %s", proj.InterfaceName(), abbreviateKey(slot.WGPublicKey))
			tw.Flush()

			if status == "running" {
				if d, closeHub, err := e.deployerQuiet(); err == nil {
					defer closeHub()
					if st, err := d.AgentStatus(slot); err == nil {
						fmt.Fprintf(out, "\nAgent      %d relay(s), %d outbound connection(s), %d inbound\n",
							st.Relays, st.OutboundConnections, st.Activity.Connections)
						if st.App != nil && st.App.Exited {
							fmt.Fprintf(out, "Application exited with code %d after %.1fs\n", st.App.Code, st.App.Duration)
						}
					} else {
						fmt.Fprintf(out, "\nAgent      unreachable: %v\n", err)
					}
				}
			}
			if status == "asleep" {
				fmt.Fprintf(out, "\nWake with  simplecloud wake %s\n", slot.Name)
			}
			return nil
		},
	}
}

func sleepLine(status string, idleSeconds int, keep bool) string {
	switch {
	case keep:
		return status + "   keep-awake"
	case idleSeconds == 0:
		return status + "   sleeping disabled"
	default:
		return fmt.Sprintf("%s   idle timeout %s", status, shortDuration(time.Duration(idleSeconds)*time.Second))
	}
}

func abbreviateKey(k string) string {
	if len(k) > 16 {
		return k[:12] + "…"
	}
	return k
}

func volumesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "volumes",
		Short: "Show disks, their slots, and who holds the write delegation",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, done, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer done()
			_, proj, err := e.loadProject(false)
			if err != nil {
				return err
			}
			vols, err := e.store.ListVolumes(proj.ID)
			if err != nil {
				return err
			}
			if flags.JSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(vols)
			}
			if len(vols) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "This project declares no volumes.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "VOLUME\tKIND\tDISK\tMOUNTED AT\tOWNER\tDELEGATION")
			for _, v := range vols {
				delegation := "-"
				if v.DiskID != "" {
					if ds, err := e.client.ListDelegations(e.ctx, v.DiskID); err == nil {
						if len(ds) == 0 {
							delegation = "none"
						} else {
							delegation = fmt.Sprintf("%d held", len(ds))
						}
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					v.Name, v.Kind, v.DiskID, v.MountPath, v.OwnerSlot, delegation)
			}
			return tw.Flush()
		},
	}
}

func execCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <service|slot> -- <command...>",
		Short: "Run a command inside a slot",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, done, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer done()
			_, proj, err := e.loadProject(false)
			if err != nil {
				return err
			}
			slots, err := e.selectSlots(proj, args[:1])
			if err != nil {
				return err
			}
			slot := slots[0]
			if slot.SandboxID == "" {
				return fmt.Errorf("%s has no sandbox yet", slot.Name)
			}
			// Running a command is a deliberate act, so waking for it is expected
			// rather than a surprise.
			if sb, err := e.client.GetSandbox(e.ctx, slot.SandboxID); err == nil && sb.Status == archil.StatusPaused {
				if _, err := e.client.ResumeSandbox(e.ctx, slot.SandboxID); err != nil {
					return err
				}
			}
			// Each argument is quoted so the sandbox's shell sees exactly what was
			// typed. Joining with spaces would let the remote shell re-parse quotes
			// and glob characters, so `exec svc -- python3 -c '...'` would break.
			res, err := e.client.Exec(e.ctx, slot.SandboxID, shellJoin(args[1:]))
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), res.Stdout)
			if res.Stderr != "" {
				fmt.Fprint(cmd.ErrOrStderr(), res.Stderr)
			}
			if res.ExitCode != nil && *res.ExitCode != 0 {
				return SilentError{fmt.Errorf("exit %d", *res.ExitCode)}
			}
			return nil
		},
	}
}

var _ = state.Slot{}

// shellJoin quotes each argument for a POSIX shell.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
