package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func lsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List every project",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, done, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			projects, err := e.store.ListProjects()
			if err != nil {
				return err
			}
			if flags.JSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(projects)
			}
			if len(projects) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No projects yet. Run simplecloud up in a directory with a Compose file.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PROJECT\tSLOTS\tRUNNING\tASLEEP\tOVERLAY\tHUB IFACE\tDIRECTORY")
			for _, p := range projects {
				slots, err := e.store.ListSlots(p.ID)
				if err != nil {
					return err
				}
				var running, asleep int
				for _, s := range slots {
					switch s.SandboxStatus {
					case "running":
						running++
					case "paused":
						asleep++
					}
				}
				fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
					p.Name, len(slots), running, asleep, p.OverlayCIDR, p.InterfaceName(), p.Dir)
			}
			return tw.Flush()
		},
	}
}
