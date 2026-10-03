//go:build integration

// Package e2e drives the built CLI the way an operator would, so what is tested is
// the real path rather than the internals.
package e2e_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cli struct {
	t    *testing.T
	bin  string
	dir  string
	home string
	envs []string
}

func setup(t *testing.T, composeBody string) *cli {
	t.Helper()
	for _, k := range []string{"ARCHIL_API_KEY", "SC_TEST_HUB", "SC_TEST_HUB_IDENTITY"} {
		if os.Getenv(k) == "" {
			t.Skipf("%s not set", k)
		}
	}
	bin, err := filepath.Abs("../../bin/simplecloud")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("build it first with scripts/build.sh: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeBody), 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	c := &cli{t: t, bin: bin, dir: dir, home: home, envs: []string{
		"ARCHIL_API_KEY=" + os.Getenv("ARCHIL_API_KEY"),
		"SIMPLECLOUD_HOME=" + home,
		"SIMPLECLOUD_HUB=" + os.Getenv("SC_TEST_HUB"),
		"SIMPLECLOUD_HUB_IDENTITY=" + os.Getenv("SC_TEST_HUB_IDENTITY"),
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}}
	// Tear down even on failure: a leaked sandbox bills until someone notices.
	t.Cleanup(func() {
		out, err := c.try(10*time.Minute, "down", "--volumes", "-y")
		if err != nil {
			t.Errorf("teardown failed, resources may be LEAKED: %v\n%s", err, out)
			return
		}
		t.Logf("torn down:\n%s", indent(out))
	})
	// A stock host has neither wireguard-tools nor nftables, and every test gets its
	// own state directory, so each one registers and bootstraps the hub for itself.
	// Bootstrap is idempotent, so repeating it costs a preflight.
	c.run(5*time.Minute, "hub", "add", os.Getenv("SC_TEST_HUB"), "--bootstrap", "--accept-new-host-key")
	return c
}

func (c *cli) try(timeout time.Duration, args ...string) (string, error) {
	c.t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Dir = c.dir
	cmd.Env = c.envs
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
		return string(out), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return string(out), fmt.Errorf("simplecloud %s did not finish within %s", strings.Join(args, " "), timeout)
	}
}

func (c *cli) run(timeout time.Duration, args ...string) string {
	c.t.Helper()
	out, err := c.try(timeout, args...)
	if err != nil {
		c.t.Fatalf("simplecloud %s failed: %v\n%s", strings.Join(args, " "), err, indent(out))
	}
	return out
}

// dumpAgentLogs prints each slot's agent log, which is where wake, tunnel, and
// mount problems surface.
func (c *cli) dumpAgentLogs() {
	c.t.Helper()
	for _, slot := range []string{"web", "postgres"} {
		out, err := c.try(3*time.Minute, "logs", slot, "--source", "agent", "--tail", "40", "--wake")
		c.t.Logf("agent log for %s (err=%v):\n%s", slot, err, indent(out))
	}
}

func indent(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("    " + l + "\n")
	}
	return b.String()
}

// pgProbe performs the Postgres SSL negotiation, which proves a real server
// answered rather than merely that a port was open.
const pgProbe = `import socket,struct,sys,time
host = sys.argv[1]
t0 = time.time()
s = socket.create_connection((host, 5432), timeout=float(sys.argv[2]))
s.sendall(struct.pack("!II", 8, 80877103))
r = s.recv(1)
s.close()
print("PGOK %s %r %.2f" % (host, r, time.time() - t0))`

// holdProbe opens a connection to postgres and keeps it, detached, so the
// connection outlives the exec session that created it.
const holdProbe = `import subprocess
script = """
import socket, struct, time
s = socket.create_connection(("postgres", 5432), timeout=25)
s.sendall(struct.pack("!II", 8, 80877103))
s.recv(1)
time.sleep(900)
"""
open("/tmp/hold.py", "w").write(script)
p = subprocess.Popen(["setsid", "python3", "/tmp/hold.py"],
                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
print("HOLDING pid", p.pid)`

const fixture = `
services:
  web:
    image: nicolaka/netshoot:latest
    command: ["python3", "-m", "http.server", "8080", "--bind", "0.0.0.0"]
    ports:
      - "8080:8080"
    depends_on: [postgres]
    x-simplecloud-idle-timeout: 45s
  postgres:
    image: postgres:17-alpine
    ports:
      - "5432:5432"
    environment:
      POSTGRES_PASSWORD: devpassword
      PGDATA: /var/lib/postgresql/data/pgdata
    volumes:
      - pgdata:/var/lib/postgresql/data
    x-simplecloud-idle-timeout: 45s
volumes:
  pgdata: {}
`

// TestProjectLifecycle is the end-to-end path: deploy, reach a service by name over
// the private network, sleep the whole stack by cascade, wake it by traffic, and
// tear it down leaving nothing behind.
func TestProjectLifecycle(t *testing.T) {
	c := setup(t, fixture)

	t.Log("deploying")
	out := c.run(12*time.Minute, "up")
	t.Log("\n" + indent(out))

	// A datastore port must not be published, or a Compose file written for local
	// development would put the database on the internet.
	if !strings.Contains(out, "5432  private") {
		t.Errorf("postgres should be private, not published:\n%s", out)
	}
	if !strings.Contains(out, "8080  public") {
		t.Errorf("web should be published:\n%s", out)
	}

	url := strings.TrimSpace(c.run(time.Minute, "url", "web"))
	if !strings.HasPrefix(url, "https://") {
		t.Fatalf("url web returned %q", url)
	}
	// url must print nothing but the URL, or `open $(simplecloud url web)` breaks.
	if strings.Contains(url, " ") || strings.Count(url, "\n") > 0 {
		t.Errorf("url should print the bare URL, got %q", url)
	}

	t.Run("public URL answers", func(t *testing.T) {
		if code := httpCode(t, url, 3*time.Minute); code != 200 {
			t.Fatalf("want 200 from %s, got %d", url, code)
		}
	})

	t.Run("reaches postgres by name over the overlay", func(t *testing.T) {
		for _, name := range []string{"postgres", "postgres-1"} {
			out := c.run(3*time.Minute, "exec", "web", "--", "python3", "-c", pgProbe, name, "30")
			if !strings.Contains(out, "PGOK") {
				t.Fatalf("%s did not answer the Postgres handshake:\n%s", name, out)
			}
			t.Logf("%s", strings.TrimSpace(out))
		}
	})

	t.Run("up is a no-op when nothing changed", func(t *testing.T) {
		out := c.run(5*time.Minute, "up")
		if !strings.Contains(out, "nothing to do") {
			t.Errorf("an unchanged project should do nothing:\n%s", out)
		}
	})

	t.Run("sleep cascades then traffic wakes it", func(t *testing.T) {
		// An open connection must prevent sleep, including an idle pool. The holder is
		// detached with setsid so the connection outlives the exec session.
		held := c.run(2*time.Minute, "exec", "web", "--", "python3", "-c", holdProbe)
		if !strings.Contains(held, "HOLDING") {
			t.Fatalf("could not hold a connection to postgres:\n%s", held)
		}
		time.Sleep(8 * time.Second)

		out := c.run(5*time.Minute, "reap", "--dry-run")
		if !strings.Contains(out, "staying awake") {
			c.dumpAgentLogs()
			t.Errorf("postgres should refuse to sleep with a connection open:\n%s", out)
		}

		// Sleeping the API drains its relay connections, which is what lets the
		// database observe a clean FIN and start its own idle timer. Pausing alone
		// freezes sockets rather than closing them.
		c.run(5*time.Minute, "sleep", "web")
		time.Sleep(10 * time.Second)
		out = c.run(3*time.Minute, "ps")
		if !strings.Contains(out, "asleep") {
			t.Errorf("web should be asleep:\n%s", out)
		}
		if strings.Contains(out, "conn") {
			t.Errorf("draining should have closed postgres's inbound connection:\n%s", out)
		}

		t.Log("waiting out postgres's idle timeout")
		deadline := time.Now().Add(5 * time.Minute)
		for {
			out = c.run(8*time.Minute, "reap")
			if strings.Contains(out, "postgres-1") && strings.Contains(out, "asleep") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("postgres never slept after web did:\n%s", out)
			}
			time.Sleep(15 * time.Second)
		}
		t.Log("the whole stack is asleep")

		// A request to the public URL resumes the API.
		start := time.Now()
		if code := httpCode(t, url, 4*time.Minute); code != 200 {
			t.Fatalf("a request should wake web, got %d", code)
		}
		t.Logf("web woke in %s", time.Since(start).Round(100*time.Millisecond))

		// The API then reaches the database by name, and the relay rings its
		// doorbell to wake it. This is what lets a private service with no public
		// port be reachable on demand.
		probe, perr := c.try(6*time.Minute, "exec", "web", "--", "python3", "-c", pgProbe, "postgres", "240")
		if perr != nil || !strings.Contains(probe, "PGOK") {
			c.dumpAgentLogs()
			t.Fatalf("postgres should have woken when web connected to it: %v\n%s", perr, probe)
		}
		out = probe
		t.Logf("postgres woke on demand: %s", strings.TrimSpace(out))
	})

	t.Run("data survived on the volume", func(t *testing.T) {
		out := c.run(3*time.Minute, "volumes")
		if !strings.Contains(out, "pgdata") || !strings.Contains(out, "dsk-") {
			t.Errorf("the volume should be backed by a disk:\n%s", out)
		}
	})
}
