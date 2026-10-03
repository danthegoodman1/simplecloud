//go:build integration

package hub_test

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danthegoodman1/simplecloud/internal/hub"
)

func dial(t *testing.T, pinned string) *hub.Client {
	t.Helper()
	target := os.Getenv("SC_TEST_HUB")
	identity := os.Getenv("SC_TEST_HUB_IDENTITY")
	if target == "" || identity == "" {
		t.Skip("SC_TEST_HUB and SC_TEST_HUB_IDENTITY not set")
	}
	user, host := "root", target
	if at := strings.LastIndex(target, "@"); at >= 0 {
		user, host = target[:at], target[at+1:]
	}
	c, err := hub.Dial(context.Background(), hub.DialOptions{
		User: user, Host: host, Port: "22", Identity: identity, PinnedHostKey: pinned,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// The gate: --bootstrap alone takes a stock host with nothing preinstalled to a
// working hub, so cloud-init is an optimization rather than a prerequisite.
func TestBootstrapFromBareHost(t *testing.T) {
	c := dial(t, "")
	ctx := context.Background()

	before, err := c.Preflight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("preflight before bootstrap: wg=%v nft=%v forwarding=%v kernel=%v problems=%v",
		before.WGTool, before.NFTables, before.Forwarding, before.KernelWireGuard, before.Problems)
	if !before.KernelWireGuard {
		t.Fatal("this kernel has no WireGuard support, which bootstrap cannot fix")
	}

	if err := c.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	after, err := c.Preflight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !after.OK() {
		t.Fatalf("preflight still failing after bootstrap: %v", after.Problems)
	}
	t.Log("bootstrap took a bare host to a passing preflight")

	// Bootstrap must be re-runnable: an operator will run `hub add` again.
	if err := c.Bootstrap(ctx); err != nil {
		t.Errorf("bootstrap is not idempotent: %v", err)
	}
}

// Host key pinning: record on first use, and refuse a change rather than trusting
// an unknown host with the project's network.
func TestHostKeyPinnedAndMismatchRefused(t *testing.T) {
	c := dial(t, "")
	key := c.HostKey()
	if key == "" {
		t.Fatal("no host key recorded on first connect")
	}
	if !strings.HasPrefix(key, "ssh-") && !strings.HasPrefix(key, "ecdsa-") {
		t.Errorf("host key looks wrong: %q", key)
	}
	// Reconnecting with the pinned key must succeed.
	dial(t, key)

	target := os.Getenv("SC_TEST_HUB")
	user, host := "root", target
	if at := strings.LastIndex(target, "@"); at >= 0 {
		user, host = target[:at], target[at+1:]
	}
	_, err := hub.Dial(context.Background(), hub.DialOptions{
		User: user, Host: host, Port: "22",
		Identity:      os.Getenv("SC_TEST_HUB_IDENTITY"),
		PinnedHostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	})
	if err == nil {
		t.Fatal("a mismatched host key must be refused")
	}
	var mismatch *hub.HostKeyMismatch
	if !asMismatch(err, &mismatch) {
		t.Fatalf("want HostKeyMismatch, got %T: %v", err, err)
	}
	if !strings.Contains(mismatch.Error(), "accept-new-host-key") {
		t.Errorf("the error should name the remedy: %v", mismatch)
	}
}

func asMismatch(err error, target **hub.HostKeyMismatch) bool {
	for err != nil {
		if m, ok := err.(*hub.HostKeyMismatch); ok {
			*target = m
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Two projects share one hub. Each gets its own interface, and the firewall must
// confine traffic to one interface or forwarding plus per-project routes bridges
// them.
func TestTwoProjectsCoexistAndAreIsolated(t *testing.T) {
	c := dial(t, "")
	ctx := context.Background()
	if err := c.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	const a, b = "wg-sctestA", "wg-sctestB"
	t.Cleanup(func() {
		c.DeleteInterface(ctx, a)
		c.DeleteInterface(ctx, b)
		c.ApplyFirewall(ctx, nil)
	})

	pubA, err := c.EnsureInterface(ctx, a, 51890, "10.88.201.1")
	if err != nil {
		t.Fatalf("ensure %s: %v", a, err)
	}
	pubB, err := c.EnsureInterface(ctx, b, 51891, "10.88.202.1")
	if err != nil {
		t.Fatalf("ensure %s: %v", b, err)
	}
	if pubA == "" || pubA == pubB {
		t.Fatalf("each interface needs its own key: %q %q", pubA, pubB)
	}
	// Re-ensure must return the same key: a project's hub identity is stable.
	if again, err := c.EnsureInterface(ctx, a, 51890, "10.88.201.1"); err != nil || again != pubA {
		t.Errorf("EnsureInterface is not idempotent: %q vs %q (%v)", again, pubA, err)
	}
	// The private key must never leave the hub.
	if strings.Contains(pubA, "PRIVATE") || len(pubA) != 44 {
		t.Errorf("expected only a public key, got %q", pubA)
	}

	peersA := []hub.Peer{
		{Name: "web-1", PublicKey: "bGlhU0hDMGtIZHNyZEVtaW5nZFBtWm9iQVNYNERvTmc=", AllowedIP: "10.88.201.10/32"},
		{Name: "db-1", PublicKey: "a1B0VlZ0YVpuWk1pRXhNVkFtaWRTVFVsdFhVa2NHQnM=", AllowedIP: "10.88.201.11/32"},
	}
	if err := c.SyncPeers(ctx, a, 51890, peersA); err != nil {
		t.Fatalf("sync peers: %v", err)
	}
	if err := c.SyncPeers(ctx, b, 51891, []hub.Peer{
		{Name: "api-1", PublicKey: "ZEhKaGMyZ3RjR0ZuWlMxb1lXNWtiR1V0ZEdWemRDMHg=", AllowedIP: "10.88.202.10/32"},
	}); err != nil {
		t.Fatalf("sync peers b: %v", err)
	}
	if err := c.ApplyFirewall(ctx, []string{a, b}); err != nil {
		t.Fatalf("firewall: %v", err)
	}

	st, err := c.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Forwarding || !st.FirewallOK {
		t.Errorf("hub state: forwarding=%v firewall=%v", st.Forwarding, st.FirewallOK)
	}
	byName := map[string]int{}
	ports := map[string]int{}
	for _, iface := range st.Interfaces {
		byName[iface.Name] = len(iface.Peers)
		ports[iface.Name] = iface.ListenPort
	}
	if byName[a] != 2 || byName[b] != 1 {
		t.Errorf("peer counts wrong: %v", byName)
	}
	// A peer sync must not disturb the listen port. wg syncconf applies a whole
	// configuration, so an incomplete one resets it and every peer then dials a
	// port nothing listens on — a tunnel that silently never handshakes.
	if ports[a] != 51890 || ports[b] != 51891 {
		t.Errorf("listen ports changed after syncing peers: %v", ports)
	}

	// Removing one peer from A must leave B untouched.
	if err := c.SyncPeers(ctx, a, 51890, peersA[:1]); err != nil {
		t.Fatal(err)
	}
	st, _ = c.Fetch(ctx)
	byName = map[string]int{}
	for _, iface := range st.Interfaces {
		byName[iface.Name] = len(iface.Peers)
	}
	if byName[a] != 1 {
		t.Errorf("peer not removed from %s: %v", a, byName)
	}
	if byName[b] != 1 {
		t.Errorf("removing a peer from %s disturbed %s: %v", a, b, byName)
	}

	// The firewall must drop by default and accept only same-interface flows. A
	// wildcard on both sides would permit exactly the cross-project traffic this
	// prevents, so the rules are inspected rather than assumed.
	rules, err := c.Sudo(ctx, "nft list table inet simplecloud")
	if err != nil {
		t.Fatalf("listing rules: %v", err)
	}
	if !regexp.MustCompile(`chain forward \{[^}]*policy drop`).MatchString(rules) {
		t.Errorf("forward chain must drop by default:\n%s", rules)
	}
	for _, name := range []string{a, b} {
		same := regexp.MustCompile(`iifname "` + name + `" oifname "` + name + `" accept`)
		if !same.MatchString(rules) {
			t.Errorf("missing same-interface accept for %s:\n%s", name, rules)
		}
	}
	// No rule may accept a flow entering one interface and leaving another.
	for _, bad := range []string{
		`iifname "` + a + `" oifname "` + b + `"`,
		`iifname "` + b + `" oifname "` + a + `"`,
		`iifname "wg-*" oifname "wg-*" accept`,
	} {
		if strings.Contains(rules, bad) {
			t.Errorf("rules permit cross-project traffic (%s):\n%s", bad, rules)
		}
	}

	// Removing a project removes only its rule.
	if err := c.ApplyFirewall(ctx, []string{a}); err != nil {
		t.Fatal(err)
	}
	rules, _ = c.Sudo(ctx, "nft list table inet simplecloud")
	if strings.Contains(rules, b) {
		t.Errorf("%s's rule survived its removal:\n%s", b, rules)
	}
	if !strings.Contains(rules, a) {
		t.Errorf("%s's rule was removed with %s:\n%s", a, b, rules)
	}
}

func TestFirewallBlocksOverlayToInternetAndHubServices(t *testing.T) {
	c := dial(t, "")
	ctx := context.Background()
	if err := c.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	const name = "wg-sctestC"
	t.Cleanup(func() { c.DeleteInterface(ctx, name); c.ApplyFirewall(ctx, nil) })
	if _, err := c.EnsureInterface(ctx, name, 51892, "10.88.203.1"); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyFirewall(ctx, []string{name}); err != nil {
		t.Fatal(err)
	}
	rules, err := c.Sudo(ctx, "nft list table inet simplecloud")
	if err != nil {
		t.Fatal(err)
	}
	// Sandboxes have their own egress and must never route through the hub, so no
	// rule may forward from the overlay to a non-wg interface.
	if regexp.MustCompile(`iifname "wg-[^"]*" oifname "(eth|ens|eno)`).MatchString(rules) {
		t.Errorf("overlay must not forward to the internet:\n%s", rules)
	}
	// The overlay reaches the hub only for the heartbeat's ICMP; everything else,
	// SSH included, is dropped.
	if !strings.Contains(rules, `iifname "wg-*" drop`) {
		t.Errorf("overlay input should be dropped apart from ICMP:\n%s", rules)
	}
	if !strings.Contains(rules, "echo-request") {
		t.Errorf("the heartbeat's ICMP must be permitted:\n%s", rules)
	}
	_ = time.Now
}
