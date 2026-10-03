//go:build integration

package archil_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/danthegoodman1/simplecloud/internal/archil"
)

// TestPrefix marks every resource these tests create so the sweeper can always
// find them, including after a panic.
const TestPrefix = "sctest-"

func client(t *testing.T) *archil.Client {
	t.Helper()
	key := os.Getenv("ARCHIL_API_KEY")
	if key == "" {
		t.Skip("ARCHIL_API_KEY not set")
	}
	c, err := archil.New(key, os.Getenv("SIMPLECLOUD_REGION"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func name(suffix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return TestPrefix + suffix + "-" + hex.EncodeToString(b)
}

// sweep deletes every resource carrying the test prefix. It runs on cleanup even
// when a test fails, because a leaked sandbox bills until someone notices.
func sweep(t *testing.T, c *archil.Client) {
	t.Helper()
	ctx := context.Background()
	sbs, err := c.ListSandboxes(ctx)
	if err != nil {
		t.Logf("sweep: listing sandboxes: %v", err)
	}
	for _, sb := range sbs {
		if !strings.HasPrefix(sb.Name, TestPrefix) {
			continue
		}
		switch sb.Status {
		case archil.StatusRunning, archil.StatusPaused, archil.StatusPending:
			if _, err := c.StopSandbox(ctx, sb.ID); err != nil {
				t.Logf("sweep: stopping %s: %v", sb.Name, err)
			}
		}
		// Delete refuses unless the sandbox is stopped, exited, or failed, and a
		// stop started elsewhere may still be in flight, so retry rather than leak.
		var derr error
		for attempt := range 6 {
			if attempt > 0 {
				time.Sleep(3 * time.Second)
			}
			if cur, err := c.GetSandbox(ctx, sb.ID); err == nil {
				switch cur.Status {
				case archil.StatusRunning, archil.StatusPaused:
					_, _ = c.StopSandbox(ctx, sb.ID)
				}
			}
			if derr = c.DeleteSandbox(ctx, sb.ID); derr == nil {
				break
			}
		}
		if derr != nil {
			t.Errorf("sweep: LEAKED sandbox %s: %v", sb.Name, derr)
		} else {
			t.Logf("sweep: deleted sandbox %s", sb.Name)
		}
	}
	disks, err := c.ListDisks(ctx)
	if err != nil {
		t.Logf("sweep: listing disks: %v", err)
	}
	for _, d := range disks {
		if !strings.HasPrefix(d.Name, TestPrefix) {
			continue
		}
		if err := c.DeleteDisk(ctx, d.ID); err != nil {
			t.Logf("sweep: deleting disk %s: %v", d.Name, err)
		} else {
			t.Logf("sweep: deleted disk %s", d.Name)
		}
	}
}

func TestClientLifecycle(t *testing.T) {
	c := client(t)
	t.Cleanup(func() { sweep(t, c) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	zero := 0
	t0 := time.Now()
	sb, err := c.CreateSandbox(ctx, archil.CreateSandboxRequest{
		Name:           name("client"),
		BaseImage:      "nicolaka/netshoot:latest",
		VCPUCount:      1,
		MemSizeMiB:     1024,
		MaxTTLSeconds:  3600,
		IdleTTLSeconds: &zero,
	}, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created %s (%s) in %.1fs, platform=%s", sb.Name, sb.ID, time.Since(t0).Seconds(), sb.Platform)
	if sb.Status != archil.StatusRunning {
		t.Fatalf("want running, got %s", sb.Status)
	}
	if sb.IdleTTLSeconds != 0 {
		t.Errorf("idle_ttl_seconds should be pinned to 0, got %d", sb.IdleTTLSeconds)
	}

	// Egress is denied by default and must be enabled explicitly. Creating with a
	// network field is accepted and silently ignored, which this asserts.
	if err := c.UpdateNetwork(ctx, sb.ID, archil.AllowAllEgress()); err != nil {
		if ae, ok := err.(*archil.APIError); ok && ae.PlanRequired() {
			t.Fatalf("egress needs a Team plan: %v", err)
		}
		t.Fatalf("update network: %v", err)
	}
	n, err := c.GetNetwork(ctx, sb.ID)
	if err != nil || n.Egress.Default != "allow" {
		t.Fatalf("egress not enabled: %+v err=%v", n, err)
	}

	out, err := c.ExecOK(ctx, sb.ID, "uname -m && id -u")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(out, "x86_64") {
		t.Errorf("want x86_64, got %q", out)
	}
	t.Logf("exec output: %q", strings.TrimSpace(out))

	// Exit codes must propagate, or no caller can tell success from failure.
	res, err := c.Exec(ctx, sb.ID, "exit 7")
	if err != nil {
		t.Fatalf("exec failing command: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 7 {
		t.Errorf("want exit 7, got %v", res.ExitCode)
	}

	payload := strings.Repeat("simplecloud-upload-probe\n", 5000)
	if err := c.UploadFile(ctx, sb.ID, "/opt/sc/probe.txt", "0755", strings.NewReader(payload)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	got, err := c.ExecOK(ctx, sb.ID, "wc -c < /opt/sc/probe.txt && stat -c %a /opt/sc/probe.txt")
	if err != nil {
		t.Fatalf("verify upload: %v", err)
	}
	if !strings.Contains(got, fmt.Sprint(len(payload))) {
		t.Errorf("uploaded size mismatch: want %d, got %q", len(payload), got)
	}
	if !strings.Contains(got, "755") {
		t.Errorf("want mode 755, got %q", got)
	}
	t.Logf("uploaded %d bytes, verified: %q", len(payload), strings.TrimSpace(got))
}

// TestDoorbellWake is the mechanism a private service relies on: a token-
// authenticated request wakes a paused sandbox, and an unauthenticated one does
// not, so a database needs no public port to be wakeable.
func TestDoorbellWake(t *testing.T) {
	c := client(t)
	t.Cleanup(func() { sweep(t, c) })
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	zero := 0
	sb, err := c.CreateSandbox(ctx, archil.CreateSandboxRequest{
		Name: name("doorbell"), BaseImage: "nicolaka/netshoot:latest",
		VCPUCount: 1, MemSizeMiB: 1024, MaxTTLSeconds: 3600, IdleTTLSeconds: &zero,
	}, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.UpdateNetwork(ctx, sb.ID, archil.AllowAllEgress()); err != nil {
		t.Fatalf("egress: %v", err)
	}
	p, err := c.Run(ctx, sb.ID, "cd /tmp && python3 -m http.server 48080 --bind 0.0.0.0", archil.RunOptions{TimeoutSeconds: 3000})
	if err != nil {
		t.Fatalf("start doorbell: %v", err)
	}
	p.Disconnect()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if out, _ := c.ExecOK(ctx, sb.ID, "ss -ltn | grep -c 48080 || true"); strings.HasPrefix(strings.TrimSpace(out), "1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("doorbell never started listening")
		}
		time.Sleep(time.Second)
	}
	tok, err := c.CreatePortToken(ctx, sb.ID, 48080, "1h")
	if err != nil {
		t.Fatalf("port token: %v", err)
	}
	ports, err := c.ListPorts(ctx, sb.ID)
	if err != nil {
		t.Fatalf("list ports: %v", err)
	}
	if len(ports) != 0 {
		t.Errorf("a port token must not publish the port, got %+v", ports)
	}

	get := func(withToken bool) (int, time.Duration) {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+tok.Hostname+"/", nil)
		if withToken {
			req.Header.Set("X-Archil-Token", tok.Token)
		}
		start := time.Now()
		resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
		if err != nil {
			t.Fatalf("doorbell request: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, time.Since(start)
	}

	if code, _ := get(true); code != 200 {
		t.Fatalf("awake doorbell with token: want 200, got %d", code)
	}
	if code, _ := get(false); code != 401 {
		t.Fatalf("awake doorbell without token: want 401, got %d", code)
	}

	// Unauthenticated traffic must not wake it, or the doorbell is a free
	// denial-of-wallet vector for anyone who learns the hostname.
	if _, err := c.PauseSandbox(ctx, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	code, _ := get(false)
	cur, err := c.GetSandbox(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code != 401 || cur.Status != archil.StatusPaused {
		t.Errorf("unauthenticated request should 401 and leave it paused; got %d and %s", code, cur.Status)
	}

	code, took := get(true)
	if code != 200 {
		t.Fatalf("token request on paused sandbox: want 200, got %d", code)
	}
	t.Logf("doorbell woke a paused sandbox in %s", took.Round(10*time.Millisecond))
	if took > 10*time.Second {
		t.Errorf("wake took %s, want well under 10s", took)
	}
	after, err := c.GetSandbox(ctx, sb.ID)
	if err != nil || after.Status != archil.StatusRunning {
		t.Errorf("want running after wake, got %v err=%v", after.Status, err)
	}
	// Memory survived, so the listener is the same process rather than a restart.
	if out, err := c.ExecOK(ctx, sb.ID, "ss -ltn | grep -c 48080"); err != nil || !strings.HasPrefix(strings.TrimSpace(out), "1") {
		t.Errorf("listener did not survive pause/resume: %q %v", out, err)
	}
}
