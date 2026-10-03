//go:build integration

package e2e_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// bg is a simplecloud command that keeps running, which forward does.
type bg struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *syncBuffer
	args []string
	done chan struct{}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (c *cli) start(args ...string) *bg {
	c.t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Dir = c.dir
	cmd.Env = c.envs
	buf := &syncBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		c.t.Fatalf("starting simplecloud %s: %v", strings.Join(args, " "), err)
	}
	b := &bg{t: c.t, cmd: cmd, out: buf, args: args, done: make(chan struct{})}
	go func() { cmd.Wait(); close(b.done) }()
	c.t.Cleanup(func() { b.stop() })
	return b
}

// waitFor blocks until the output contains want, so a test talks to a forward only
// once it is listening.
func (b *bg) waitFor(want string, timeout time.Duration) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(b.out.String(), want) {
			return
		}
		select {
		case <-b.done:
			b.t.Fatalf("simplecloud %s exited before printing %q:\n%s",
				strings.Join(b.args, " "), want, indent(b.out.String()))
		case <-time.After(100 * time.Millisecond):
		}
	}
	b.t.Fatalf("simplecloud %s did not print %q within %s:\n%s",
		strings.Join(b.args, " "), want, timeout, indent(b.out.String()))
}

// waitExit expects the command to stop on its own, which is how a refused forward
// reports itself.
func (b *bg) waitExit(timeout time.Duration) string {
	b.t.Helper()
	select {
	case <-b.done:
		return b.out.String()
	case <-time.After(timeout):
		b.t.Fatalf("simplecloud %s was still running after %s:\n%s",
			strings.Join(b.args, " "), timeout, indent(b.out.String()))
		return ""
	}
}

func (b *bg) stop() string {
	if b.cmd.Process != nil {
		b.cmd.Process.Signal(syscall.SIGINT)
	}
	select {
	case <-b.done:
	case <-time.After(20 * time.Second):
		if b.cmd.Process != nil {
			b.cmd.Process.Kill()
		}
		<-b.done
	}
	return b.out.String()
}

// freePort asks the kernel for a port nothing is using, so parallel forwards in
// one test run do not collide on a hardcoded number.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// pgSSLRequest performs the Postgres SSL negotiation against a local address.
//
// Postgres answers this before authenticating, so a reply of S or N proves a real
// server is on the far end of the forward rather than merely an open local port.
func pgSSLRequest(addr string, timeout time.Duration) (byte, error) {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	req := make([]byte, 8)
	binary.BigEndian.PutUint32(req[0:4], 8)
	binary.BigEndian.PutUint32(req[4:8], 80877103) // the SSLRequest code
	if _, err := c.Write(req); err != nil {
		return 0, err
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(c, reply); err != nil {
		return 0, err
	}
	return reply[0], nil
}

func assertPostgresAnswers(t *testing.T, addr string) {
	t.Helper()
	reply, err := pgSSLRequest(addr, 30*time.Second)
	if err != nil {
		t.Fatalf("Postgres SSL negotiation through %s failed: %v", addr, err)
	}
	if reply != 'S' && reply != 'N' {
		t.Fatalf("Postgres replied %q through %s, wanted S or N", reply, addr)
	}
	t.Logf("Postgres answered %q through %s", reply, addr)
}

// TestForwardReachesAPrivateDatabase is the point of the command: postgres has no
// published port, so this is the only way a local tool reaches it.
func TestForwardReachesAPrivateDatabase(t *testing.T) {
	c := setup(t, fixture)
	t.Log(c.run(20*time.Minute, "up", "-y"))

	// A private port stays private. If it were published, the forward would be
	// proving nothing.
	endpoints := c.run(2*time.Minute, "endpoints")
	if !strings.Contains(endpoints, "postgres") || !strings.Contains(endpoints, "private") {
		t.Fatalf("postgres should be private, so a forward is the only way in:\n%s", indent(endpoints))
	}

	t.Run("explicit local port", func(t *testing.T) {
		local := freePort(t)
		f := c.start("forward", fmt.Sprintf("%d:postgres:5432", local))
		f.waitFor("Press Ctrl-C", 3*time.Minute)
		assertPostgresAnswers(t, fmt.Sprintf("127.0.0.1:%d", local))
		t.Logf("forward output:\n%s", indent(f.stop()))
	})

	t.Run("port discovered from the agent", func(t *testing.T) {
		// Naming no port asks the agent which it declares, so this also proves the
		// status endpoint reports them.
		f := c.start("forward", "postgres")
		f.waitFor("Press Ctrl-C", 3*time.Minute)
		out := f.out.String()
		if !strings.Contains(out, "5432") {
			t.Fatalf("forward should have discovered 5432 from the agent:\n%s", indent(out))
		}
		assertPostgresAnswers(t, "127.0.0.1:5432")
		t.Logf("forward output:\n%s", indent(f.stop()))
	})

	t.Run("several connections over one forward", func(t *testing.T) {
		local := freePort(t)
		f := c.start("forward", fmt.Sprintf("%d:postgres:5432", local))
		f.waitFor("Press Ctrl-C", 3*time.Minute)
		addr := fmt.Sprintf("127.0.0.1:%d", local)
		for i := 0; i < 3; i++ {
			assertPostgresAnswers(t, addr)
		}
		// Each connection opens its own tunnel, so all three should be reported.
		if n := strings.Count(f.out.String(), "connected"); n < 3 {
			t.Fatalf("3 connections produced %d tunnels:\n%s", n, indent(f.out.String()))
		}
		t.Logf("forward output:\n%s", indent(f.stop()))
	})

	t.Run("an undeclared port is refused", func(t *testing.T) {
		local := freePort(t)
		f := c.start("forward", fmt.Sprintf("%d:postgres:22", local))
		out := f.waitExit(3 * time.Minute)
		if !strings.Contains(out, "does not declare port 22") {
			t.Fatalf("forwarding an undeclared port should be refused with a reason:\n%s", indent(out))
		}
		t.Logf("refused as expected:\n%s", indent(out))
	})

	t.Run("a jump host reaches it through another service", func(t *testing.T) {
		// web resolves postgres through its own relay, so forwarding through web
		// exercises the overlay path as well as the ingress one.
		local := freePort(t)
		f := c.start("forward", "--via", "web", "--any", fmt.Sprintf("%d:postgres:5432", local))
		f.waitFor("Press Ctrl-C", 3*time.Minute)
		assertPostgresAnswers(t, fmt.Sprintf("127.0.0.1:%d", local))
		t.Logf("forward output:\n%s", indent(f.stop()))
	})
}

// TestForwardWakesASleepingDatabase checks the property that makes forward usable
// on a stack that sleeps: connecting is itself the wake.
func TestForwardWakesASleepingDatabase(t *testing.T) {
	c := setup(t, fixture)
	t.Log(c.run(20*time.Minute, "up", "-y"))

	t.Log(c.run(5*time.Minute, "sleep"))
	ps := c.run(2*time.Minute, "ps")
	if strings.Contains(ps, "running") {
		t.Fatalf("the stack should be asleep before testing a wake:\n%s", indent(ps))
	}
	t.Logf("asleep:\n%s", indent(ps))

	local := freePort(t)
	start := time.Now()
	f := c.start("forward", fmt.Sprintf("%d:postgres:5432", local))
	// Resolving the port wakes the slot, so this line already means it is awake.
	f.waitFor("Press Ctrl-C", 5*time.Minute)
	t.Logf("a sleeping database was reachable %s after asking", time.Since(start).Round(100*time.Millisecond))

	assertPostgresAnswers(t, fmt.Sprintf("127.0.0.1:%d", local))

	ps = c.run(2*time.Minute, "ps")
	if !strings.Contains(ps, "running") {
		t.Fatalf("postgres should be running after a forward woke it:\n%s", indent(ps))
	}
	t.Logf("awake after the forward:\n%s", indent(ps))
	t.Logf("forward output:\n%s", indent(f.stop()))
}

// TestForwardHoldsASlotAwake checks that the reaper leaves a slot alone while an
// operator is using it, which it would not do if a tunnel were invisible to the
// activity count.
func TestForwardHoldsASlotAwake(t *testing.T) {
	c := setup(t, fixture)
	t.Log(c.run(20*time.Minute, "up", "-y"))

	local := freePort(t)
	f := c.start("forward", fmt.Sprintf("%d:postgres:5432", local))
	f.waitFor("Press Ctrl-C", 3*time.Minute)

	// Hold a connection open rather than reconnecting, so only the open tunnel can
	// account for the slot staying awake.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), 30*time.Second)
	if err != nil {
		t.Fatalf("opening a held connection: %v", err)
	}
	defer conn.Close()

	// The fixture's idle timeout is 45s, so wait past it and ask the reaper to act.
	time.Sleep(70 * time.Second)
	out := c.run(5*time.Minute, "reap", "--dry-run")
	if strings.Contains(out, "postgres") && strings.Contains(out, "sleep") {
		t.Fatalf("the reaper wanted to sleep a slot with a forward open:\n%s", indent(out))
	}
	t.Logf("reaper left it alone:\n%s", indent(out))

	// The held connection should show up as a forwarded connection on the slot.
	status := c.run(2*time.Minute, "show", "postgres")
	if !strings.Contains(status, "1 forwarded") {
		t.Fatalf("show should report the open forward:\n%s", indent(status))
	}
	t.Logf("status:\n%s", indent(status))
	t.Logf("forward output:\n%s", indent(f.stop()))
}
