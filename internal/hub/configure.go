package hub

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Preflight reports what the hub is missing. Each problem names its own remedy,
// and --bootstrap applies all of them.
type Preflight struct {
	KernelWireGuard bool
	WGTool          bool
	NFTables        bool
	Forwarding      bool
	ConfigDirSafe   bool
	Problems        []string
}

func (p *Preflight) OK() bool { return len(p.Problems) == 0 }

func (c *Client) Preflight(ctx context.Context) (*Preflight, error) {
	p := &Preflight{}
	// A kernel without WireGuard cannot be fixed by installing a package, so it is
	// checked first and reported differently from a missing tool.
	if out, err := c.Sudo(ctx, `modprobe wireguard 2>/dev/null; ls /sys/module/wireguard >/dev/null 2>&1 && echo yes || echo no`); err == nil {
		p.KernelWireGuard = strings.Contains(out, "yes")
	}
	if !p.KernelWireGuard {
		// Some kernels have it built in without exposing /sys/module.
		if _, err := c.Sudo(ctx, `ip link add dev scprobe type wireguard && ip link del scprobe`); err == nil {
			p.KernelWireGuard = true
		}
	}
	if !p.KernelWireGuard {
		p.Problems = append(p.Problems, "the kernel has no WireGuard support (a current Ubuntu or Debian kernel includes it)")
	}
	if _, err := c.Run(ctx, "command -v wg"); err == nil {
		p.WGTool = true
	} else {
		p.Problems = append(p.Problems, "wg is not installed (apt-get install wireguard-tools)")
	}
	if _, err := c.Run(ctx, "command -v nft"); err == nil {
		p.NFTables = true
	} else {
		p.Problems = append(p.Problems, "nft is not installed (apt-get install nftables)")
	}
	if out, err := c.Sudo(ctx, "sysctl -n net.ipv4.ip_forward"); err == nil && strings.TrimSpace(out) == "1" {
		p.Forwarding = true
	} else {
		p.Problems = append(p.Problems, "IPv4 forwarding is off (sysctl -w net.ipv4.ip_forward=1)")
	}
	if out, err := c.Sudo(ctx, `stat -c %a /etc/wireguard 2>/dev/null || echo missing`); err == nil {
		mode := strings.TrimSpace(out)
		p.ConfigDirSafe = mode == "700"
		if !p.ConfigDirSafe {
			p.Problems = append(p.Problems, "/etc/wireguard is not mode 700 (it holds private keys)")
		}
	}
	return p, nil
}

// Bootstrap installs and configures what preflight found missing, taking a stock
// host to a working hub. Cloud-init is therefore an optimization rather than a
// prerequisite.
func (c *Client) Bootstrap(ctx context.Context) error {
	steps := []struct{ name, cmd string }{
		{"installing wireguard-tools and nftables",
			`export DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null; then
  apt-get update -qq
  apt-get install -y -qq wireguard-tools nftables
else
  echo "no apt-get; install wireguard-tools and nftables yourself" >&2; exit 1
fi`},
		{"enabling IPv4 forwarding",
			`sysctl -w net.ipv4.ip_forward=1 >/dev/null
printf 'net.ipv4.ip_forward=1\n' > /etc/sysctl.d/99-simplecloud.conf`},
		{"securing /etc/wireguard",
			`mkdir -p /etc/wireguard && chmod 700 /etc/wireguard`},
	}
	for _, s := range steps {
		if _, err := c.Sudo(ctx, s.cmd); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return nil
}

// Interface describes one project's WireGuard interface on the hub.
type Interface struct {
	Name       string
	ListenPort int
	PublicKey  string
	Peers      []Peer
}

type Peer struct {
	Name      string
	PublicKey string
	AllowedIP string
}

// EnsureInterface creates the interface if absent and returns its public key.
//
// The private key is generated on the hub and never leaves it: the CLI reads only
// the public half.
func (c *Client) EnsureInterface(ctx context.Context, name string, listenPort int, overlayIP string) (string, error) {
	keyPath := "/etc/wireguard/" + name + ".key"
	script := fmt.Sprintf(`set -eu
umask 077
mkdir -p /etc/wireguard
if [ ! -s %s ]; then wg genkey > %s; fi
wg pubkey < %s`, shellQuote(keyPath), shellQuote(keyPath), shellQuote(keyPath))
	pub, err := c.Sudo(ctx, script)
	if err != nil {
		return "", fmt.Errorf("generating the hub key for %s: %w", name, err)
	}
	pub = strings.TrimSpace(pub)

	// Bring the interface up idempotently. `ip link add` fails when it exists, so
	// the existence check keeps re-apply a no-op rather than an error.
	up := fmt.Sprintf(`set -eu
if ! ip link show %s >/dev/null 2>&1; then
  ip link add dev %s type wireguard
fi
wg set %s listen-port %d private-key %s
ip -4 addr replace %s/24 dev %s
ip link set up dev %s`,
		shellQuote(name), shellQuote(name), shellQuote(name), listenPort, shellQuote(keyPath),
		overlayIP, shellQuote(name), shellQuote(name))
	if _, err := c.Sudo(ctx, up); err != nil {
		return "", fmt.Errorf("bringing up %s: %w", name, err)
	}
	return pub, nil
}

// SyncPeers converges the interface's peer set on exactly the given peers, adding
// and removing as needed without disturbing unrelated interfaces.
//
// The configuration it applies is complete, including the listen port and private
// key. `wg syncconf` applies a whole configuration: an [Interface] section that
// omits them resets the listen port to a random one and clears the key, so peers
// would then be dialing a port nothing listens on. The key is interpolated on the
// hub from its own key file, so it still never leaves the host.
func (c *Client) SyncPeers(ctx context.Context, name string, listenPort int, peers []Peer) error {
	var fragment strings.Builder
	for _, p := range peers {
		fmt.Fprintf(&fragment, "\n[Peer]\n# %s\nPublicKey = %s\nAllowedIPs = %s\n", p.Name, p.PublicKey, p.AllowedIP)
	}
	peersPath := "/etc/wireguard/" + name + ".peers"
	if err := c.WriteFile(ctx, peersPath, "600", fragment.String()); err != nil {
		return err
	}
	confPath := "/etc/wireguard/" + name + ".conf"
	keyPath := "/etc/wireguard/" + name + ".key"
	script := fmt.Sprintf(`set -eu
umask 077
{
  printf '[Interface]\nPrivateKey = %%s\nListenPort = %%s\n' "$(cat %s)" %d
  cat %s
} > %s
wg syncconf %s %s
# A reset listen port means peers would dial a port nothing listens on, which
# presents as a tunnel that never handshakes. Fail here instead.
got=$(wg show %s listen-port)
if [ "$got" != "%d" ]; then
  echo "listen port is $got after syncconf, expected %d" >&2
  exit 1
fi`,
		shellQuote(keyPath), listenPort, shellQuote(peersPath), shellQuote(confPath),
		shellQuote(name), shellQuote(confPath), shellQuote(name), listenPort, listenPort)
	if _, err := c.Sudo(ctx, script); err != nil {
		return fmt.Errorf("syncing peers on %s: %w", name, err)
	}
	return nil
}

func (c *Client) DeleteInterface(ctx context.Context, name string) error {
	script := fmt.Sprintf(`set -eu
ip link del dev %s 2>/dev/null || true
rm -f /etc/wireguard/%s.key /etc/wireguard/%s.peers`, shellQuote(name), name, name)
	_, err := c.Sudo(ctx, script)
	return err
}

// ApplyFirewall replaces the whole simplecloud nftables table from the full set of
// interfaces. Full replacement is convergent by construction, so there is no
// partial state to reconcile.
//
// This is what makes a shared hub safe. Forwarding plus a route for every
// project's subnet would otherwise bridge projects: a packet from one project
// addressed to another arrives, passes WireGuard's source check because its own
// source is allowed, and is routed straight out the other interface. Destinations
// are not validated on ingress, so only a same-interface rule prevents it.
func (c *Client) ApplyFirewall(ctx context.Context, interfaces []string) error {
	var b strings.Builder
	b.WriteString("table inet simplecloud\ndelete table inet simplecloud\n")
	b.WriteString("table inet simplecloud {\n")
	b.WriteString("  chain forward {\n    type filter hook forward priority 0; policy drop;\n")
	for _, name := range interfaces {
		// Same interface in and out. A wildcard on both sides would permit exactly
		// the cross-project traffic this exists to stop.
		fmt.Fprintf(&b, "    iifname %q oifname %q accept\n", name, name)
	}
	b.WriteString("  }\n")
	b.WriteString("  chain input {\n    type filter hook input priority 0; policy accept;\n")
	// The agent's heartbeat pings the hub's overlay address, which is what restores
	// a tunnel quickly after a resume, so ICMP echo has to be allowed.
	b.WriteString("    iifname \"wg-*\" icmp type { echo-request, echo-reply } accept\n")
	b.WriteString("    iifname \"wg-*\" drop\n")
	b.WriteString("  }\n}\n")

	if err := c.WriteFile(ctx, "/etc/nftables.d/simplecloud.nft", "600", b.String()); err != nil {
		return err
	}
	if _, err := c.Sudo(ctx, "nft -f /etc/nftables.d/simplecloud.nft"); err != nil {
		return fmt.Errorf("applying the hub firewall: %w", err)
	}
	return nil
}

// Status is the hub's live view, for comparing against local state.
type Status struct {
	Interfaces []InterfaceStatus
	Forwarding bool
	WGVersion  string
	FirewallOK bool
}

type InterfaceStatus struct {
	Name       string
	ListenPort int
	PublicKey  string
	Peers      []PeerStatus
}

type PeerStatus struct {
	PublicKey     string
	AllowedIPs    string
	Endpoint      string
	LastHandshake time.Time
	RxBytes       int64
	TxBytes       int64
}

// Fetch reads every simplecloud interface and its peers in one round trip.
func (c *Client) Fetch(ctx context.Context) (*Status, error) {
	s := &Status{}
	if out, err := c.Run(ctx, "wg --version"); err == nil {
		s.WGVersion = strings.TrimSpace(out)
	}
	if out, err := c.Sudo(ctx, "sysctl -n net.ipv4.ip_forward"); err == nil {
		s.Forwarding = strings.TrimSpace(out) == "1"
	}
	if _, err := c.Sudo(ctx, "nft list table inet simplecloud >/dev/null 2>&1"); err == nil {
		s.FirewallOK = true
	}
	// `wg show all dump` is tab-separated: the first line per interface is the
	// interface itself, the rest are its peers.
	out, err := c.Sudo(ctx, "wg show all dump")
	if err != nil {
		return s, nil
	}
	byName := map[string]*InterfaceStatus{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		name := f[0]
		iface, seen := byName[name]
		if !seen {
			iface = &InterfaceStatus{Name: name}
			byName[name] = iface
			order = append(order, name)
			// interface line: name, private, public, listen-port, fwmark
			if len(f) >= 5 {
				iface.PublicKey = f[2]
				iface.ListenPort, _ = strconv.Atoi(f[3])
			}
			continue
		}
		// peer line: name, public, preshared, endpoint, allowed-ips, handshake, rx, tx, keepalive
		ps := PeerStatus{PublicKey: f[1]}
		if len(f) >= 5 {
			ps.Endpoint = f[3]
			ps.AllowedIPs = f[4]
		}
		if len(f) >= 6 {
			if secs, err := strconv.ParseInt(f[5], 10, 64); err == nil && secs > 0 {
				ps.LastHandshake = time.Unix(secs, 0)
			}
		}
		if len(f) >= 8 {
			ps.RxBytes, _ = strconv.ParseInt(f[6], 10, 64)
			ps.TxBytes, _ = strconv.ParseInt(f[7], 10, 64)
		}
		iface.Peers = append(iface.Peers, ps)
	}
	for _, name := range order {
		if strings.HasPrefix(name, "wg-") {
			s.Interfaces = append(s.Interfaces, *byName[name])
		}
	}
	return s, nil
}
