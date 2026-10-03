package cli

import (
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/agent"
	"github.com/danthegoodman1/simplecloud/internal/archil"
)

func logsCmd() *cobra.Command {
	var follow, timestamps, noPrefix, wake bool
	var tail int
	var source string
	c := &cobra.Command{
		Use:   "logs [service...]",
		Short: "Show service output, interleaved per slot",
		Long: "Prints the last 200 lines by default, then exits. With -f it prints the backlog\n" +
			"first and then streams, so you land with context already on screen.\n\n" +
			"A sleeping slot is read from locally stored history rather than woken: any\n" +
			"connection to a paused sandbox resumes it, and spending a cold start to print\n" +
			"text is rarely what you wanted. Use --wake or -f to resume it deliberately.",
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
			prefix := !noPrefix && len(slots) > 1

			d, closeHub, err := e.deployerQuiet()
			if err != nil {
				return err
			}
			defer closeHub()

			type pending struct {
				slot  string
				lines []agent.Line
			}
			var batches []pending
			for _, s := range slots {
				awake := false
				if s.SandboxID != "" {
					if sb, err := e.client.GetSandbox(e.ctx, s.SandboxID); err == nil {
						awake = sb.Status == archil.StatusRunning
					}
				}
				if !awake && !(wake || follow) {
					lines := e.readHistory(proj, s.Name, tail)
					if len(lines) > 0 {
						last := lines[len(lines)-1].TS.Format("15:04:05")
						fmt.Fprintf(out, "%s is asleep; showing stored output to %s — use -f or --wake for live\n", s.Name, last)
					} else {
						fmt.Fprintf(out, "%s is asleep and no output has been stored yet — use --wake for live\n", s.Name)
					}
					batches = append(batches, pending{s.Name, lines})
					continue
				}
				if !awake {
					if _, err := e.client.ResumeSandbox(e.ctx, s.SandboxID); err != nil {
						fmt.Fprintf(out, "%s: %v\n", s.Name, err)
						continue
					}
				}
				e.pullLogs(d, s)
				batches = append(batches, pending{s.Name, e.readHistory(proj, s.Name, tail)})
			}

			// Interleave by timestamp so a multi-service view reads in order.
			type stamped struct {
				slot string
				line agent.Line
			}
			var merged []stamped
			for _, b := range batches {
				for _, l := range b.lines {
					merged = append(merged, stamped{b.slot, l})
				}
			}
			sort.SliceStable(merged, func(i, j int) bool { return merged[i].line.TS.Before(merged[j].line.TS) })
			if tail > 0 && len(merged) > tail {
				merged = merged[len(merged)-tail:]
			}
			for _, m := range merged {
				fmt.Fprintln(out, formatLine(m.slot, m.line, timestamps, prefix))
			}

			for _, s := range slots {
				if st, err := d.AgentStatus(s); err == nil && st.App != nil && st.App.Exited {
					fmt.Fprintf(out, "%s: the application exited with code %d after %.1fs\n",
						s.Name, st.App.Code, st.App.Duration)
				}
			}
			if !follow {
				return nil
			}
			fmt.Fprintln(out, "--- following ---")
			for {
				for _, s := range slots {
					before := s.LogCursor
					e.pullLogs(d, s)
					if s.LogCursor == before {
						continue
					}
					for _, l := range e.readHistory(proj, s.Name, 50) {
						if l.TS.After(time.Now().Add(-10 * time.Second)) {
							fmt.Fprintln(out, formatLine(s.Name, l, timestamps, prefix))
						}
					}
				}
				select {
				case <-e.ctx.Done():
					return nil
				case <-time.After(2 * time.Second):
				}
			}
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "print the backlog, then stream")
	c.Flags().IntVar(&tail, "tail", 200, "lines to show; 0 for everything stored")
	c.Flags().BoolVarP(&timestamps, "timestamps", "t", false, "include timestamps")
	c.Flags().BoolVar(&noPrefix, "no-prefix", false, "omit the per-slot prefix")
	c.Flags().BoolVar(&wake, "wake", false, "resume a sleeping slot to read live output")
	c.Flags().StringVar(&source, "source", "", "read the agent's own log instead (agent)")
	return c
}
