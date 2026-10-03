package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// The agent configures the overlay through netlink and the WireGuard kernel API
// rather than by running wg and ip.
//
// It has to: the application's image decides what binaries exist, and most do not
// ship networking tools. postgres:17-alpine has no wg, so a shell-out agent works
// on a debug image and fails on a real one.
const overlayInterface = "wg0"

func (a *Agent) SetupNetwork(ctx context.Context) error {
	if err := a.writeHosts(); err != nil {
		return err
	}
	if err := a.bringUpWireGuard(); err != nil {
		return err
	}
	a.applyFirewall(ctx)
	return nil
}

// writeHosts maps each peer service to its loopback alias rather than its overlay
// address. Resolving a name therefore reaches a local relay that knows which
// service and port was wanted, which is what makes wake-on-connect possible
// without the hub inspecting traffic.
func (a *Agent) writeHosts() error {
	const marker = "# simplecloud: managed, do not edit below"
	raw, err := os.ReadFile("/etc/hosts")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, marker) {
			break
		}
		kept = append(kept, line)
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(strings.Join(kept, "\n"), "\n"))
	b.WriteString("\n" + marker + "\n")
	for _, p := range a.cfg.Peers {
		names := p.Aliases
		if len(names) == 0 {
			names = []string{p.Service, p.Slot}
		}
		fmt.Fprintf(&b, "%s\t%s\n", p.LoopbackIP, strings.Join(names, " "))
	}
	// A service reaches itself by name too, so configuration naming the service
	// keeps working inside it.
	fmt.Fprintf(&b, "%s\t%s %s\n", a.cfg.OverlayIP, a.cfg.Service, a.cfg.Slot)
	return os.WriteFile("/etc/hosts", []byte(b.String()), 0o644)
}

func (a *Agent) bringUpWireGuard() error {
	priv, err := wgtypes.ParseKey(a.cfg.WGPrivateKey)
	if err != nil {
		return fmt.Errorf("parsing the slot's WireGuard key: %w", err)
	}
	hubKey, err := wgtypes.ParseKey(a.cfg.Hub.PublicKey)
	if err != nil {
		return fmt.Errorf("parsing the hub's public key: %w", err)
	}

	link, err := netlink.LinkByName(overlayInterface)
	if err != nil {
		// A cold boot starts with no interface; a resume already has one.
		attrs := netlink.NewLinkAttrs()
		attrs.Name = overlayInterface
		if err := netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: attrs}); err != nil {
			return fmt.Errorf("creating %s: %w", overlayInterface, err)
		}
		if link, err = netlink.LinkByName(overlayInterface); err != nil {
			return err
		}
	}

	client, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("opening the WireGuard control socket: %w", err)
	}
	defer client.Close()

	endpoint, err := net.ResolveUDPAddr("udp", a.cfg.Hub.Endpoint)
	if err != nil {
		return fmt.Errorf("resolving the hub endpoint %s: %w", a.cfg.Hub.Endpoint, err)
	}
	// AllowedIPs is the whole project range: every peer is reached through the hub,
	// because no sandbox has an inbound UDP endpoint of its own.
	_, projectNet, err := net.ParseCIDR(a.cfg.OverlayCIDR)
	if err != nil {
		return fmt.Errorf("parsing the project range %s: %w", a.cfg.OverlayCIDR, err)
	}
	keepalive := 15 * time.Second
	if err := client.ConfigureDevice(overlayInterface, wgtypes.Config{
		PrivateKey:   &priv,
		ReplacePeers: true,
		Peers: []wgtypes.PeerConfig{{
			PublicKey:                   hubKey,
			Endpoint:                    endpoint,
			PersistentKeepaliveInterval: &keepalive,
			ReplaceAllowedIPs:           true,
			AllowedIPs:                  []net.IPNet{*projectNet},
		}},
	}); err != nil {
		return fmt.Errorf("configuring %s: %w", overlayInterface, err)
	}

	addr, err := netlink.ParseAddr(a.cfg.OverlayIP + "/24")
	if err != nil {
		return err
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("assigning %s: %w", addr, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bringing up %s: %w", overlayInterface, err)
	}
	return nil
}

// applyFirewall restricts the overlay to the project's own range and declared
// ports. nftables is often absent from an application image, so a failure here
// degrades rather than failing the deploy: the hub's own policy already confines
// traffic to one project, and AllowedIPs bounds what can arrive.
func (a *Agent) applyFirewall(ctx context.Context) {
	rules := a.firewallRules()
	path := StateDir + "/nftables.conf"
	if err := os.WriteFile(path, []byte(rules), 0o600); err != nil {
		a.logf("could not write firewall rules: %v", err)
		return
	}
	if out, err := runIfPresent(ctx, "nft", "-f", path); err != nil {
		a.logf("firewall not applied (%v: %s); the hub still confines traffic to this project",
			err, strings.TrimSpace(out))
	}
}

func (a *Agent) firewallRules() string {
	var b strings.Builder
	b.WriteString("table inet simplecloud\ndelete table inet simplecloud\n")
	b.WriteString("table inet simplecloud {\n  chain input {\n")
	b.WriteString("    type filter hook input priority 0; policy accept;\n")
	fmt.Fprintf(&b, "    iifname != %q accept\n", overlayInterface)
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    icmp type { echo-request, echo-reply } accept\n")
	for _, port := range a.cfg.ReachablePorts {
		fmt.Fprintf(&b, "    ip saddr %s tcp dport %d accept\n", a.cfg.OverlayCIDR, port)
	}
	fmt.Fprintf(&b, "    iifname %q drop\n  }\n}\n", overlayInterface)
	return b.String()
}

// Heartbeat sends a packet into the tunnel every interval.
//
// WireGuard handshakes on outbound send, so any packet whose destination falls
// inside the peer's AllowedIPs is enough — the hub dropping it is irrelevant. A
// resumed sandbox does not re-handshake promptly on its own, and the hub cannot
// provoke it because the sandbox is the side behind NAT. Without this, recovery
// takes about 16s regardless of persistent-keepalive; with it, about 1.4s.
func (a *Agent) Heartbeat(ctx context.Context) {
	target := &net.UDPAddr{IP: net.ParseIP(a.cfg.Hub.OverlayIP), Port: 9}
	t := time.NewTicker(a.cfg.HeartbeatInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if conn, err := net.DialUDP("udp", nil, target); err == nil {
				conn.Write([]byte{0})
				conn.Close()
			}
		}
	}
}

// WaitForOverlay blocks until the tunnel has handshaked, so readiness means
// reachable. The device's own handshake time is authoritative and needs no reply,
// which matters because the hub drops everything from the overlay but ICMP.
func (a *Agent) WaitForOverlay(ctx context.Context, timeout time.Duration) error {
	client, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer client.Close()
	target := &net.UDPAddr{IP: net.ParseIP(a.cfg.Hub.OverlayIP), Port: 9}
	deadline := time.Now().Add(timeout)
	for {
		// Provoke a handshake rather than waiting for the heartbeat's next tick.
		if conn, err := net.DialUDP("udp", nil, target); err == nil {
			conn.Write([]byte{0})
			conn.Close()
		}
		if dev, err := client.Device(overlayInterface); err == nil {
			for _, p := range dev.Peers {
				if !p.LastHandshakeTime.IsZero() {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the tunnel to the hub never handshaked within %s.\n"+
				"  If this persists, inbound UDP to the hub may be blocked", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// HandshakeAge reports how long ago the tunnel last handshaked, for status output.
func (a *Agent) HandshakeAge() (time.Duration, bool) {
	client, err := wgctrl.New()
	if err != nil {
		return 0, false
	}
	defer client.Close()
	dev, err := client.Device(overlayInterface)
	if err != nil {
		return 0, false
	}
	for _, p := range dev.Peers {
		if !p.LastHandshakeTime.IsZero() {
			return time.Since(p.LastHandshakeTime), true
		}
	}
	return 0, false
}

var _ = base64.StdEncoding
