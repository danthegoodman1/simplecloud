package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/hub"
	"github.com/danthegoodman1/simplecloud/internal/state"
)

func hubCmd() *cobra.Command {
	c := &cobra.Command{Use: "hub", Short: "Register and inspect the WireGuard hub"}
	c.AddCommand(hubAddCmd(), hubStatusCmd(), hubSetEndpointCmd())
	return c
}

// connectHub dials the configured hub, pinning its host key on first use and
// refusing a change unless the operator says the host was rebuilt deliberately.
func (e *env) connectHub(acceptNewKey bool) (*hub.Client, *state.Hub, error) {
	if err := e.cfg.RequireHub(); err != nil {
		return nil, nil, err
	}
	rec, err := e.store.GetHub()
	if err != nil {
		return nil, nil, err
	}
	pinned := ""
	if rec != nil && !acceptNewKey && rec.SSHTarget == e.cfg.Hub {
		pinned = rec.HostKey
	}
	user, host, port := e.cfg.SSHTarget()
	client, err := hub.Dial(e.ctx, hub.DialOptions{
		User: user, Host: host, Port: port,
		Identity: e.cfg.HubIdentity, PinnedHostKey: pinned,
	})
	if err != nil {
		var mismatch *hub.HostKeyMismatch
		if errors.As(err, &mismatch) {
			return nil, nil, mismatch
		}
		return nil, nil, err
	}
	if rec == nil {
		rec = &state.Hub{}
	}
	rec.SSHTarget = e.cfg.Hub
	rec.Endpoint = e.cfg.Endpoint()
	rec.HostKey = client.HostKey()
	return client, rec, nil
}

func hubAddCmd() *cobra.Command {
	var bootstrap, acceptNewKey, save bool
	c := &cobra.Command{
		Use:   "add <ssh-target>",
		Short: "Register the hub, pin its host key, and optionally prepare it",
		Long: "Registers a Linux host as the WireGuard hub. With --bootstrap it installs\n" +
			"wireguard-tools and nftables, enables forwarding, and writes the base firewall,\n" +
			"so a stock host with nothing preinstalled becomes a working hub.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				flags.Hub = args[0]
			}
			e, done, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			out := cmd.OutOrStdout()

			client, rec, err := e.connectHub(acceptNewKey)
			if err != nil {
				return err
			}
			defer client.Close()
			fmt.Fprintf(out, "connected to %s\n", client.Target())
			fmt.Fprintf(out, "host key    %s\n", abbreviate(client.HostKey()))

			pf, err := client.Preflight(e.ctx)
			if err != nil {
				return err
			}
			if !pf.KernelWireGuard {
				return fmt.Errorf("this host's kernel has no WireGuard support, which cannot be installed.\n  Use a current Ubuntu or Debian image")
			}
			if !pf.OK() {
				if !bootstrap {
					var b strings.Builder
					b.WriteString("the hub is not ready:\n")
					for _, p := range pf.Problems {
						b.WriteString("    - " + p + "\n")
					}
					b.WriteString("  Re-run with --bootstrap to fix all of these.")
					return fmt.Errorf("%s", b.String())
				}
				fmt.Fprintln(out, "bootstrapping:")
				for _, p := range pf.Problems {
					fmt.Fprintf(out, "  - %s\n", p)
				}
				if err := client.Bootstrap(e.ctx); err != nil {
					return err
				}
				if pf, err = client.Preflight(e.ctx); err != nil {
					return err
				}
				if !pf.OK() {
					return fmt.Errorf("the hub is still not ready after bootstrap:\n    - %s", strings.Join(pf.Problems, "\n    - "))
				}
			}
			rec.Bootstrapped = true
			if err := e.store.PutHub(rec); err != nil {
				return err
			}
			// Re-apply the firewall from the full project list, which is convergent
			// and also installs the base table on a freshly bootstrapped host.
			if err := e.applyHubFirewall(client); err != nil {
				return err
			}
			fmt.Fprintf(out, "endpoint    %s\n", rec.Endpoint)
			fmt.Fprintln(out, "preflight   ready (kernel WireGuard, wg, nft, forwarding, /etc/wireguard mode 700)")
			if save {
				if err := e.cfg.Save(); err != nil {
					return err
				}
				fmt.Fprintf(out, "saved       %s/config.toml\n", e.cfg.Home)
			}
			fmt.Fprintln(out, "\nThe hub is ready. Run simplecloud up in a project directory.")
			if port := firstListenPortHint(); port != "" {
				fmt.Fprintf(out, "If a firewall sits in front of this host, open inbound UDP %s.\n", port)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&bootstrap, "bootstrap", false, "install and configure whatever preflight finds missing")
	c.Flags().BoolVar(&acceptNewKey, "accept-new-host-key", false, "replace the pinned SSH host key, for a host rebuilt deliberately")
	c.Flags().BoolVar(&save, "save", true, "write these settings to config.toml")
	return c
}

func firstListenPortHint() string {
	return fmt.Sprintf("%d-%d", state.FirstListenPort, state.LastListenPort)
}

// applyHubFirewall rebuilds the whole table from every project, so the rule set
// always matches local state rather than accumulating.
func (e *env) applyHubFirewall(client *hub.Client) error {
	projects, err := e.store.ListProjects()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(projects))
	for _, p := range projects {
		names = append(names, p.InterfaceName())
	}
	return client.ApplyFirewall(e.ctx, names)
}

func hubStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the hub's interfaces, peers, and last handshakes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, done, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			client, rec, err := e.connectHub(false)
			if err != nil {
				return err
			}
			defer client.Close()
			st, err := client.Fetch(e.ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Hub        %s   endpoint %s\n", rec.SSHTarget, rec.Endpoint)
			fmt.Fprintf(out, "WireGuard  %s   forwarding %s   firewall %s\n",
				strings.TrimSpace(st.WGVersion), yesNo(st.Forwarding), yesNo(st.FirewallOK))
			fmt.Fprintf(out, "Host key   %s\n\n", abbreviate(rec.HostKey))

			projects, err := e.store.ListProjects()
			if err != nil {
				return err
			}
			byIface := map[string]*state.Project{}
			for _, p := range projects {
				byIface[p.InterfaceName()] = p
			}
			if len(st.Interfaces) == 0 {
				fmt.Fprintln(out, "No project interfaces yet.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "INTERFACE\tPROJECT\tPORT\tPEERS\tLAST HANDSHAKE")
			for _, iface := range st.Interfaces {
				project := "(unknown to this machine)"
				if p, ok := byIface[iface.Name]; ok {
					project = p.Name
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n",
					iface.Name, project, iface.ListenPort, len(iface.Peers), handshakeSummary(iface.Peers))
			}
			tw.Flush()

			// A peer that never handshakes is the symptom of a blocked UDP port, so
			// name that rather than leaving it to be guessed.
			var never int
			for _, iface := range st.Interfaces {
				for _, p := range iface.Peers {
					if p.LastHandshake.IsZero() {
						never++
					}
				}
			}
			if never > 0 {
				fmt.Fprintf(out, "\n%d peer(s) have never completed a handshake.\n"+
					"  If that persists, inbound UDP %s may be blocked in front of the hub.\n",
					never, firstListenPortHint())
			}
			return nil
		},
	}
}

func hubSetEndpointCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-endpoint <host>",
		Short: "Change the address sandboxes dial",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, done, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			rec, err := e.store.GetHub()
			if err != nil {
				return err
			}
			if rec == nil {
				return fmt.Errorf("no hub registered yet.\n  Run simplecloud hub add <target> --bootstrap")
			}
			rec.Endpoint = args[0]
			if err := e.store.PutHub(rec); err != nil {
				return err
			}
			e.cfg.HubEndpoint = args[0]
			if err := e.cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "endpoint is now %s\n"+
				"Run simplecloud up to push it to every agent; agents dial out and hold the endpoint themselves.\n", args[0])
			return nil
		},
	}
}

func handshakeSummary(peers []hub.PeerStatus) string {
	if len(peers) == 0 {
		return "-"
	}
	var newest time.Time
	var never int
	for _, p := range peers {
		if p.LastHandshake.IsZero() {
			never++
			continue
		}
		if p.LastHandshake.After(newest) {
			newest = p.LastHandshake
		}
	}
	switch {
	case never == len(peers):
		return "never"
	case never > 0:
		return fmt.Sprintf("%s ago (%d never)", shortDuration(time.Since(newest)), never)
	default:
		return shortDuration(time.Since(newest)) + " ago"
	}
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func yesNo(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func abbreviate(key string) string {
	f := strings.Fields(key)
	if len(f) < 2 {
		return key
	}
	blob := f[1]
	if len(blob) > 24 {
		blob = blob[:12] + "…" + blob[len(blob)-8:]
	}
	return f[0] + " " + blob
}
