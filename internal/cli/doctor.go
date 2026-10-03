package cli

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/archil"
	"github.com/danthegoodman1/simplecloud/internal/compose"
)

// doctor pre-empts the failures that present as something other than their cause.
// Each check names the remedy, because the whole point is to answer the question
// before it becomes a debugging session.
func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check credentials, the hub, and the build toolchain",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, done, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			out := cmd.OutOrStdout()
			var problems []string
			check := func(name string, ok bool, detail, fix string) {
				mark := "ok  "
				if !ok {
					mark = "FAIL"
					problems = append(problems, name+": "+fix)
				}
				fmt.Fprintf(out, "  %s  %-34s %s\n", mark, name, detail)
			}

			if e.cfg.APIKey == "" {
				check("Archil credential", false, "not set",
					"Set ARCHIL_API_KEY")
			} else {
				client, err := archil.New(e.cfg.APIKey, e.cfg.Region)
				if err != nil {
					check("Archil credential", false, err.Error(), "Check SIMPLECLOUD_REGION")
				} else if _, err := client.ListSandboxes(e.ctx); err != nil {
					check("Archil credential", false, err.Error(), "Check ARCHIL_API_KEY")
				} else {
					check("Archil credential", true, "accepted in "+client.Region(), "")
				}
			}

			if e.cfg.Hub == "" {
				check("Hub", false, "not configured",
					"Set SIMPLECLOUD_HUB, or run simplecloud hub add <target> --bootstrap")
			} else {
				client, _, err := e.connectHub(false)
				if err != nil {
					check("Hub reachable", false, err.Error(), "Check SIMPLECLOUD_HUB and your SSH identity")
				} else {
					defer client.Close()
					check("Hub reachable", true, client.Target(), "")
					pf, err := client.Preflight(e.ctx)
					switch {
					case err != nil:
						check("Hub preflight", false, err.Error(), "Re-run hub add --bootstrap")
					case !pf.KernelWireGuard:
						check("Hub kernel WireGuard", false, "absent",
							"Use a current Ubuntu or Debian image; this cannot be installed")
					case !pf.OK():
						check("Hub preflight", false, strings.Join(pf.Problems, "; "),
							"Run simplecloud hub add --bootstrap")
					default:
						check("Hub preflight", true, "wg, nft, forwarding, /etc/wireguard 700", "")
					}
					st, err := client.Fetch(e.ctx)
					if err == nil {
						never := 0
						for _, iface := range st.Interfaces {
							for _, p := range iface.Peers {
								if p.LastHandshake.IsZero() {
									never++
								}
							}
						}
						check("Hub firewall", st.FirewallOK, "simplecloud table present",
							"Run simplecloud up to reapply it")
						if never > 0 {
							check("Peer handshakes", false,
								fmt.Sprintf("%d peer(s) have never handshaked", never),
								fmt.Sprintf("Open inbound UDP %s in front of the hub", firstListenPortHint()))
						} else if len(st.Interfaces) > 0 {
							check("Peer handshakes", true, "all peers have handshaked", "")
						}
					}
				}
			}

			// Builds happen here, not in the sandbox, so the toolchain is only needed
			// by a project that builds.
			needsBuild := false
			if cp, _, err := e.loadProject(false); err == nil {
				for _, svc := range cp.Services {
					if svc.Build != nil {
						needsBuild = true
					}
				}
			}
			if needsBuild {
				if _, err := exec.LookPath("docker"); err != nil {
					check("docker buildx", false, "docker not found",
						"Install Docker; a project with build: needs it to produce a linux/amd64 image")
				} else {
					out, err := exec.Command("docker", "buildx", "version").CombinedOutput()
					check("docker buildx", err == nil, strings.TrimSpace(string(out)),
						"Install buildx; amd64 emulation is needed on an arm64 machine")
				}
				if e.cfg.Registry == "" {
					check("Registry", false, "not set",
						"Set SIMPLECLOUD_REGISTRY to a registry you can push to")
				} else {
					check("Registry", true, e.cfg.Registry, "")
				}
			}

			fmt.Fprintln(out)
			if len(problems) == 0 {
				fmt.Fprintln(out, "Everything checks out.")
				return nil
			}
			fmt.Fprintf(out, "%d problem(s):\n", len(problems))
			for _, p := range problems {
				fmt.Fprintln(out, "  - "+p)
			}
			return SilentError{fmt.Errorf("%d check(s) failed", len(problems))}
		},
	}
}

func reconcileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Compare local state against Archil and the hub, and repair drift",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, done, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer done()
			cp, proj, err := e.loadProject(false)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var repairs int

			slots, err := e.store.ListSlots(proj.ID)
			if err != nil {
				return err
			}
			// A sandbox deleted outside the CLI leaves a slot pointing at nothing.
			// Clearing the reference lets up recreate it, keeping the slot's identity.
			for _, s := range slots {
				if s.SandboxID == "" {
					continue
				}
				if _, err := e.client.GetSandbox(e.ctx, s.SandboxID); err != nil {
					fmt.Fprintf(out, "  %s: its sandbox is gone; clearing the reference\n", s.Name)
					s.SandboxID, s.SandboxStatus = "", ""
					if err := e.store.PutSlot(s); err != nil {
						return err
					}
					repairs++
				}
			}

			client, _, err := e.connectHub(false)
			if err != nil {
				return err
			}
			defer client.Close()
			st, err := client.Fetch(e.ctx)
			if err != nil {
				return err
			}
			found := map[string]int{}
			for _, iface := range st.Interfaces {
				found[iface.Name] = len(iface.Peers)
			}
			if _, ok := found[proj.InterfaceName()]; !ok {
				fmt.Fprintf(out, "  %s: the hub interface is missing\n", proj.InterfaceName())
				repairs++
			} else if found[proj.InterfaceName()] != len(slots) {
				fmt.Fprintf(out, "  %s: the hub has %d peer(s) for %d slot(s)\n",
					proj.InterfaceName(), found[proj.InterfaceName()], len(slots))
				repairs++
			}
			if !st.FirewallOK {
				fmt.Fprintln(out, "  the hub's firewall table is missing")
				repairs++
			}

			if repairs == 0 {
				fmt.Fprintln(out, "Local state, Archil, and the hub agree.")
				return nil
			}
			fmt.Fprintf(out, "\nRepairing %d item(s) by converging the project.\n", repairs)
			d, closeHub, err := e.deployer(cmd)
			if err != nil {
				return err
			}
			defer closeHub()
			pl, err := e.buildPlan(cp, proj)
			if err != nil {
				return err
			}
			return d.Up(cp, proj, pl, nil)
		},
	}
}

var _ = compose.CloudProfile
