package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/archil"
	"github.com/danthegoodman1/simplecloud/internal/deploy"
	"github.com/danthegoodman1/simplecloud/internal/state"
)

func sleepCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sleep [service...]",
		Short: "Pause services now, without waiting for their idle timeout",
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
			d, closeHub, err := e.deployerQuiet()
			if err != nil {
				return err
			}
			defer closeHub()
			slots, err := e.selectSlots(proj, args)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, s := range slots {
				if s.SandboxID == "" {
					continue
				}
				sb, err := e.client.GetSandbox(e.ctx, s.SandboxID)
				if err != nil || sb.Status != archil.StatusRunning {
					fmt.Fprintf(out, "%-16s already asleep\n", s.Name)
					continue
				}
				if err := e.pauseSlot(d, s); err != nil {
					fmt.Fprintf(out, "%-16s %v\n", s.Name, err)
					continue
				}
				fmt.Fprintf(out, "%-16s asleep\n", s.Name)
			}
			return nil
		},
	}
}

// pauseSlot drains before pausing.
//
// Pausing snapshots memory and stops the VM; it does not close sockets. Without
// draining first, a peer keeps seeing ESTABLISHED for as long as TCP keepalive
// takes, so a database behind a sleeping API would never go idle.
func (e *env) pauseSlot(d *deploy.Deployer, s *state.Slot) error {
	if _, err := d.AgentDrain(s); err != nil {
		// A drain failure is worth reporting but not worth refusing to sleep: the
		// reaper discounts connections from paused peers as a backstop.
		fmt.Fprintf(d.Out, "  %s: could not drain before pausing: %v\n", s.Name, err)
	}
	// Pull the final log chunk before pausing, so reading a sleeping slot's logs
	// later serves the cache rather than waking it.
	e.pullLogs(d, s)
	sb, err := e.client.PauseSandbox(e.ctx, s.SandboxID)
	if err != nil {
		return err
	}
	s.SandboxStatus = string(sb.Status)
	return e.store.PutSlot(s)
}

func wakeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wake [service...]",
		Short: "Resume services now",
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
			out := cmd.OutOrStdout()
			for _, s := range slots {
				if s.SandboxID == "" {
					continue
				}
				start := time.Now()
				sb, err := e.client.GetSandbox(e.ctx, s.SandboxID)
				if err != nil {
					fmt.Fprintf(out, "%-16s %v\n", s.Name, err)
					continue
				}
				switch sb.Status {
				case archil.StatusRunning:
					fmt.Fprintf(out, "%-16s already running\n", s.Name)
					continue
				case archil.StatusPaused:
					sb, err = e.client.ResumeSandbox(e.ctx, s.SandboxID)
				default:
					sb, err = e.client.StartSandbox(e.ctx, s.SandboxID)
				}
				if err != nil {
					fmt.Fprintf(out, "%-16s %v\n", s.Name, err)
					continue
				}
				s.SandboxStatus = string(sb.Status)
				_ = e.store.PutSlot(s)
				fmt.Fprintf(out, "%-16s running (%s)\n", s.Name, time.Since(start).Round(100*time.Millisecond))
			}
			return nil
		},
	}
}

func reapCmd() *cobra.Command {
	var all, dry bool
	c := &cobra.Command{
		Use:   "reap",
		Short: "Pause every slot past its idle timeout",
		Long: "Asks each running slot for its activity and pauses the idle ones. This is the\n" +
			"cron target: it also pulls new log data on the same round trip, so the cron\n" +
			"cadence becomes the log capture cadence.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, done, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer done()
			var projects []*state.Project
			if all {
				if projects, err = e.store.ListProjects(); err != nil {
					return err
				}
			} else {
				_, proj, err := e.loadProject(false)
				if err != nil {
					return err
				}
				projects = []*state.Project{proj}
			}
			d, closeHub, err := e.deployerQuiet()
			if err != nil {
				return err
			}
			defer closeHub()
			d.Out = cmd.OutOrStdout()
			out := cmd.OutOrStdout()

			for _, proj := range projects {
				slots, err := e.store.ListSlots(proj.ID)
				if err != nil {
					return err
				}
				// Which slots are paused matters before anything else: a connection from
				// a paused peer is not evidence of activity, because pausing freezes
				// sockets rather than closing them.
				paused := map[string]bool{}
				live := map[string]archil.SandboxStatus{}
				for _, s := range slots {
					if s.SandboxID == "" {
						continue
					}
					sb, err := e.client.GetSandbox(e.ctx, s.SandboxID)
					if err != nil {
						continue
					}
					live[s.Name] = sb.Status
					if sb.Status == archil.StatusPaused {
						paused[s.Name] = true
					}
					if string(sb.Status) != s.SandboxStatus {
						s.SandboxStatus = string(sb.Status)
						_ = e.store.PutSlot(s)
					}
				}
				for _, s := range slots {
					// A paused slot is skipped rather than asked: asking would wake it.
					if live[s.Name] != archil.StatusRunning {
						continue
					}
					act, err := d.AgentActivity(s)
					if err != nil {
						fmt.Fprintf(out, "%-16s unreachable: %v\n", s.Name, err)
						continue
					}
					// The activity poll already reaches every running slot, so the log
					// pull rides the same round trip rather than costing another.
					e.pullLogs(d, s)

					switch {
					case act.KeepAwake:
						fmt.Fprintf(out, "%-16s keep-awake\n", s.Name)
					case !act.ShouldSleep && act.Connections > 0:
						fmt.Fprintf(out, "%-16s %d connection(s), staying awake\n", s.Name, act.Connections)
					case !act.ShouldSleep:
						fmt.Fprintf(out, "%-16s idle %s of %s\n", s.Name,
							shortDuration(time.Duration(act.IdleFor)*time.Second),
							shortDuration(time.Duration(act.IdleTimeout)*time.Second))
					case dry:
						fmt.Fprintf(out, "%-16s would sleep (idle %s)\n", s.Name,
							shortDuration(time.Duration(act.IdleFor)*time.Second))
					default:
						if err := e.pauseSlot(d, s); err != nil {
							fmt.Fprintf(out, "%-16s %v\n", s.Name, err)
							continue
						}
						fmt.Fprintf(out, "%-16s asleep (idle %s)\n", s.Name,
							shortDuration(time.Duration(act.IdleFor)*time.Second))
					}
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "every project")
	c.Flags().BoolVar(&dry, "dry-run", false, "report what would sleep without pausing anything")
	return c
}

// selectSlots resolves service or slot names to slots, defaulting to all of them.
func (e *env) selectSlots(proj *state.Project, names []string) ([]*state.Slot, error) {
	slots, err := e.store.ListSlots(proj.ID)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return slots, nil
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []*state.Slot
	for _, s := range slots {
		if want[s.Name] || want[s.Service] {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		var known []string
		for _, s := range slots {
			known = append(known, s.Name)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("no slot matches %s.\n  Known slots: %s",
			strings.Join(names, ", "), strings.Join(known, ", "))
	}
	return out, nil
}
