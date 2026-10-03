package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigValidationNamesWhatIsMissing(t *testing.T) {
	full := func() *Config {
		return &Config{
			Slot: "web-1", OverlayIP: "10.88.1.10", WGPrivateKey: "k",
			Hub:          Hub{PublicKey: "p", Endpoint: "1.2.3.4:51820"},
			Command:      []string{"sh"},
			DoorbellPort: 48080, ControlToken: "t",
		}
	}
	if err := full().Validate(); err != nil {
		t.Fatalf("a complete config should validate: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"slot":          func(c *Config) { c.Slot = "" },
		"overlay":       func(c *Config) { c.OverlayIP = "" },
		"WireGuard":     func(c *Config) { c.WGPrivateKey = "" },
		"hub":           func(c *Config) { c.Hub.PublicKey = "" },
		"command":       func(c *Config) { c.Command = nil },
		"doorbell":      func(c *Config) { c.DoorbellPort = 0 },
		"control token": func(c *Config) { c.ControlToken = "" },
	} {
		c := full()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("a config missing its %s should not validate", name)
		}
	}
}

func TestConfigRoundTrip(t *testing.T) {
	c := &Config{
		Slot: "db-1", Service: "db", OverlayIP: "10.88.1.11", WGPrivateKey: "k",
		Hub:     Hub{PublicKey: "p", Endpoint: "h:51820", OverlayIP: "10.88.1.1"},
		Command: []string{"postgres"}, DoorbellPort: 48080, ControlToken: "t",
		Peers: []Peer{{Service: "web", LoopbackIP: "127.0.1.10", Ports: []int{80}}},
	}
	raw, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.json")
	os.WriteFile(path, raw, 0o600)
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slot != "db-1" || len(got.Peers) != 1 || got.Peers[0].LoopbackIP != "127.0.1.10" {
		t.Errorf("round trip lost fields: %+v", got)
	}
	// A one-second heartbeat is what turns a ~16s overlay recovery into ~1.4s, so
	// the default must not be absent.
	if got.HeartbeatInterval().Seconds() != 1 {
		t.Errorf("heartbeat default: %v", got.HeartbeatInterval())
	}
}

// The on-disk format is framed so shipping can carry the timestamp and stream.
func TestRingWritesFramedLines(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir, "svc", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, l := range []struct{ stream, text string }{
		{"stdout", "listening on 8080"},
		{"stderr", "a warning"},
	} {
		if err := r.Write(l.stream, l.text); err != nil {
			t.Fatal(err)
		}
	}
	r.Flush()
	lines, dropped, _, err := r.Read(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped {
		t.Error("nothing should have been dropped")
	}
	if len(lines) != 2 || lines[0].Text != "listening on 8080" || lines[1].Stream != "stderr" {
		t.Fatalf("lines: %+v", lines)
	}
	if lines[0].TS.IsZero() {
		t.Error("a line needs a timestamp, or a sink cannot ship it")
	}
	// The format must be one JSON object per line, so a sink can stream it.
	raw, _ := os.ReadFile(filepath.Join(dir, "svc.1.jsonl"))
	for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		var probe Line
		if err := json.Unmarshal([]byte(l), &probe); err != nil {
			t.Errorf("line is not JSON: %q", l)
		}
	}
}

// The cap always wins. An unshipped sink may delay rotation inside it, but
// suspending rotation is exactly how a broken sink fills a disk.
func TestRingStaysUnderItsCapAndReportsDrops(t *testing.T) {
	dir := t.TempDir()
	const cap = 256 << 10
	r, err := OpenRing(dir, "chatty", cap)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	line := strings.Repeat("x", 512)
	for range 4000 {
		if err := r.Write("stdout", line); err != nil {
			t.Fatal(err)
		}
	}
	r.Flush()

	var total int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if st, err := e.Info(); err == nil {
			total += st.Size()
		}
	}
	// Allow one segment of slack: the active segment is capped on its next write.
	if total > cap+cap/defaultSegments {
		t.Errorf("ring grew to %d bytes with a cap of %d", total, cap)
	}
	if r.Dropped() == 0 {
		t.Error("dropping segments must be recorded, or a gap is silent")
	}
	// Reading from before the earliest surviving segment must report the gap.
	_, dropped, _, err := r.Read(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !dropped {
		t.Error("a read starting before the surviving data should report a gap")
	}
}

func TestRingReopensAndAppends(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir, "svc", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	r.Write("stdout", "before")
	r.Close()

	again, err := OpenRing(dir, "svc", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.Write("stdout", "after")
	again.Flush()
	lines, _, _, _ := again.Read(0, 0)
	if len(lines) != 2 || lines[0].Text != "before" || lines[1].Text != "after" {
		t.Errorf("reopening should append, got %+v", lines)
	}
}

// Injected variables are facts about where a service runs, so they must be present
// and must not be shadowed by the resolved environment.
func TestInjectedEnvironment(t *testing.T) {
	cfg := &Config{
		Project: "myapp", Service: "db", Slot: "db-1", Ordinal: 2,
		OverlayIP: "10.88.3.11", Region: "aws-us-east-1",
		Env: map[string]string{"PGDATA": "/var/lib/postgresql/data"},
	}
	got := map[string]string{}
	for _, kv := range environ(cfg) {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, want := range map[string]string{
		"SIMPLECLOUD":               "1",
		"SIMPLECLOUD_PROJECT":       "myapp",
		"SIMPLECLOUD_SERVICE":       "db",
		"SIMPLECLOUD_SLOT":          "db-1",
		"SIMPLECLOUD_REPLICA_INDEX": "2",
		"SIMPLECLOUD_OVERLAY_IP":    "10.88.3.11",
		"SIMPLECLOUD_REGION":        "aws-us-east-1",
		"PGDATA":                    "/var/lib/postgresql/data",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// Outbound connections are not activity for the service that opens them, or a
// service holding an idle pool would look busy forever and never sleep.
func TestActivityCountsNothingWithoutDeclaredPorts(t *testing.T) {
	a := &Agent{cfg: &Config{Slot: "w-1", Service: "w", IdleSeconds: 60}}
	if n := a.countInbound(); n != 0 {
		t.Errorf("a service with no declared ports has no observable activity, got %d", n)
	}
}

func TestShouldSleepHonoursKeepAwakeAndTimeout(t *testing.T) {
	mk := func(keep bool, idle int) Activity {
		a := &Agent{cfg: &Config{Slot: "s-1", KeepAwake: keep, IdleSeconds: idle}}
		a.lastActive = a.lastActive.Add(-time.Hour)
		return a.Snapshot()
	}
	if mk(true, 60).ShouldSleep {
		t.Error("keep-awake must never sleep")
	}
	if mk(false, 0).ShouldSleep {
		t.Error("a disabled idle timeout must never sleep")
	}
	if !mk(false, 60).ShouldSleep {
		t.Error("an idle service past its timeout should sleep")
	}
}
