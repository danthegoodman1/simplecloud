package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/archil"
)

func downCmd() *cobra.Command {
	var volumes, yes bool
	c := &cobra.Command{
		Use:   "down",
		Short: "Destroy the project: sandboxes and hub peers",
		Long: "Removes every sandbox and the project's hub interface. Disks are kept unless\n" +
			"--volumes is given, because losing data to a typo is unrecoverable.\n\n" +
			"To pause services instead, use simplecloud sleep.",
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
			out := cmd.OutOrStdout()
			if !yes {
				vols, _ := e.store.ListVolumes(proj.ID)
				fmt.Fprintf(out, "This destroys project %s.\n", proj.Name)
				if volumes && len(vols) > 0 {
					fmt.Fprintf(out, "It also deletes %d disk(s), which cannot be undone.\n", len(vols))
				} else if len(vols) > 0 {
					fmt.Fprintf(out, "%d disk(s) will be kept; pass --volumes to delete them too.\n", len(vols))
				}
				fmt.Fprint(out, "Type the project name to confirm: ")
				var typed string
				fmt.Fscanln(cmd.InOrStdin(), &typed)
				if typed != proj.Name {
					return fmt.Errorf("not confirmed")
				}
			}

			slots, err := e.store.ListSlots(proj.ID)
			if err != nil {
				return err
			}
			for _, s := range slots {
				if s.SandboxID == "" {
					continue
				}
				if sb, err := e.client.GetSandbox(e.ctx, s.SandboxID); err == nil {
					switch sb.Status {
					case archil.StatusRunning, archil.StatusPaused:
						if _, err := e.client.StopSandbox(e.ctx, s.SandboxID); err != nil {
							fmt.Fprintf(out, "  stopping %s: %v\n", s.Name, err)
						}
					}
				}
				if err := e.client.DeleteSandbox(e.ctx, s.SandboxID); err != nil {
					fmt.Fprintf(out, "  deleting %s: %v\n", s.Name, err)
					continue
				}
				fmt.Fprintf(out, "  removed sandbox for %s\n", s.Name)
			}

			if client, _, err := e.connectHub(false); err == nil {
				defer client.Close()
				if err := client.DeleteInterface(e.ctx, proj.InterfaceName()); err != nil {
					fmt.Fprintf(out, "  removing %s: %v\n", proj.InterfaceName(), err)
				} else {
					fmt.Fprintf(out, "  removed hub interface %s\n", proj.InterfaceName())
				}
			}

			vols, _ := e.store.ListVolumes(proj.ID)
			if volumes {
				for _, v := range vols {
					if v.DiskID == "" {
						continue
					}
					if err := e.client.DeleteDisk(e.ctx, v.DiskID); err != nil {
						fmt.Fprintf(out, "  deleting disk %s: %v\n", v.DiskID, err)
						continue
					}
					fmt.Fprintf(out, "  deleted disk %s (%s)\n", v.Name, v.DiskID)
				}
			} else if len(vols) > 0 {
				fmt.Fprintf(out, "  kept %d disk(s); a later up reattaches them\n", len(vols))
			}

			if err := e.store.DeleteProject(proj.ID); err != nil {
				return err
			}
			// Rebuild the firewall without this project's rule.
			if client, _, err := e.connectHub(false); err == nil {
				defer client.Close()
				_ = e.applyHubFirewall(client)
			}
			fmt.Fprintf(out, "project %s destroyed\n", proj.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&volumes, "volumes", false, "also delete the project's disks")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}
