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

func (c *cli) start(t *testing.T, args ...string) *bg {
	t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Dir = c.dir
	cmd.Env = c.envs
	buf := &syncBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting simplecloud %s: %v", strings.Join(args, " "), err)
	}
	b := &bg{t: t, cmd: cmd, out: buf, args: args, done: make(chan struct{})}
	go func() { cmd.Wait(); close(b.done) }()
	t.Cleanup(func() { b.stop() })
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
	t.Log(c.run(20*time.Minute, "up"))

	// A private port stays private. If it were published, the forward would be
	// proving nothing.
	endpoints := c.run(2*time.Minute, "endpoints")
	if !strings.Contains(endpoints, "postgres") || !strings.Contains(endpoints, "private") {
		t.Fatalf("postgres should be private, so a forward is the only way in:\n%s", indent(endpoints))
	}

	t.Run("explicit local port", func(t *testing.T) {
		local := freePort(t)
		f := c.start(t, "forward", fmt.Sprintf("%d:postgres:5432", local))
		f.waitFor("Press Ctrl-C", 3*time.Minute)
		assertPostgresAnswers(t, fmt.Sprintf("127.0.0.1:%d", local))
		t.Logf("forward output:\n%s", indent(f.stop()))
	})

	t.Run("port discovered from the agent", func(t *testing.T) {
		// Naming no port asks the agent which it declares, so this also proves the
		// status endpoint reports them.
		f := c.start(t, "forward", "postgres")
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
		f := c.start(t, "forward", fmt.Sprintf("%d:postgres:5432", local))
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

	t.Run("an undeclared port is refused before binding", func(t *testing.T) {
		local := freePort(t)
		f := c.start(t, "forward", fmt.Sprintf("%d:postgres:22", local))
		out := f.waitExit(3 * time.Minute)
		if !strings.Contains(out, "does not declare port 22") {
			t.Fatalf("forwarding an undeclared port should be refused with a reason:\n%s", indent(out))
		}
		// Refusing after binding would leave a forward that looks healthy and fails
		// only when something connects.
		if strings.Contains(out, "Press Ctrl-C") {
			t.Fatalf("the local port was bound before the target was checked:\n%s", indent(out))
		}
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", local)); err != nil {
			t.Fatalf("the local port was left bound after the refusal: %v", err)
		} else {
			ln.Close()
		}
		t.Logf("refused as expected:\n%s", indent(out))
	})

	t.Run("any forwards an undeclared port deliberately", func(t *testing.T) {
		// The refusal is a guard rail rather than a wall, so --any gets past it.
		local := freePort(t)
		f := c.start(t, "forward", "--any", fmt.Sprintf("%d:postgres:5432", local))
		f.waitFor("Press Ctrl-C", 3*time.Minute)
		assertPostgresAnswers(t, fmt.Sprintf("127.0.0.1:%d", local))
		t.Logf("forward output:\n%s", indent(f.stop()))
	})

	t.Run("a jump host reaches it through another service", func(t *testing.T) {
		// web resolves postgres through its own relay, so forwarding through web
		// exercises the overlay path as well as the ingress one.
		local := freePort(t)
		f := c.start(t, "forward", "--via", "web", "--any", fmt.Sprintf("%d:postgres:5432", local))
		f.waitFor("Press Ctrl-C", 3*time.Minute)
		assertPostgresAnswers(t, fmt.Sprintf("127.0.0.1:%d", local))
		t.Logf("forward output:\n%s", indent(f.stop()))
	})
}

// TestForwardWakesASleepingDatabase checks the property that makes forward usable
// on a stack that sleeps: connecting is itself the wake.
func TestForwardWakesASleepingDatabase(t *testing.T) {
	c := setup(t, fixture)
	t.Log(c.run(20*time.Minute, "up"))

	t.Log(c.run(5*time.Minute, "sleep"))
	ps := c.run(2*time.Minute, "ps")
	if strings.Contains(ps, "running") {
		t.Fatalf("the stack should be asleep before testing a wake:\n%s", indent(ps))
	}
	t.Logf("asleep:\n%s", indent(ps))

	local := freePort(t)
	start := time.Now()
	f := c.start(t, "forward", fmt.Sprintf("%d:postgres:5432", local))
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

// echoFixture exists because the property under test needs a server that holds an
// idle connection. Postgres closes an unauthenticated one after its 60s
// authentication_timeout, which looks exactly like a tunnel failing.
const echoFixture = `
services:
  echo:
    image: nicolaka/netshoot:latest
    command: ["socat", "TCP-LISTEN:9000,fork,reuseaddr", "EXEC:/bin/cat"]
    expose:
      - "9000"
    x-simplecloud-idle-timeout: 45s
  web:
    image: nicolaka/netshoot:latest
    command: ["python3", "-m", "http.server", "8080", "--bind", "0.0.0.0"]
    ports:
      - "8080:8080"
    x-simplecloud-idle-timeout: 45s
`

// TestForwardHoldsASlotAwake checks that the reaper leaves a slot alone while an
// operator is using it, which it would not do if an open tunnel were invisible to
// the activity count.
func TestForwardHoldsASlotAwake(t *testing.T) {
	c := setup(t, echoFixture)
	t.Log(c.run(20*time.Minute, "up"))

	local := freePort(t)
	f := c.start(t, "forward", fmt.Sprintf("%d:echo:9000", local))
	f.waitFor("Press Ctrl-C", 3*time.Minute)

	// Open one connection and hold it idle. Only the open tunnel can account for
	// the slot staying awake, because nothing is sent after this exchange.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), 30*time.Second)
	if err != nil {
		t.Fatalf("opening a held connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("writing to the held connection: %v", err)
	}
	echoed := make([]byte, 5)
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("the held connection never reached the service: %v", err)
	}
	conn.SetReadDeadline(time.Time{})
	t.Logf("held connection established, echoed %q", echoed)

	// The fixture's idle timeout is 45s, so wait past it and ask the reaper to act.
	time.Sleep(70 * time.Second)
	out := c.run(5*time.Minute, "reap", "--dry-run")

	// web is the control. It has no forward, so it must be eligible, which is what
	// proves the idle window really elapsed rather than the test being early.
	var webLine, echoLine string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "web-1"):
			webLine = line
		case strings.Contains(line, "echo-1"):
			echoLine = line
		}
	}
	if !strings.Contains(webLine, "would sleep") {
		t.Fatalf("web should be eligible to sleep after 70s, so the window elapsed:\n%s", indent(out))
	}
	if strings.Contains(echoLine, "would sleep") {
		t.Fatalf("the reaper wanted to sleep a slot with a forward open:\n%s", indent(out))
	}
	t.Logf("web was eligible and echo was not:\n%s", indent(out))

	// The held connection should still be counted, and still work.
	status := c.run(2*time.Minute, "show", "echo")
	if !strings.Contains(status, "1 forwarded") {
		t.Fatalf("show should report the open forward:\n%s", indent(status))
	}
	if _, err := conn.Write([]byte("again")); err != nil {
		t.Fatalf("the held connection died during the idle window: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("the held connection stopped carrying traffic after 70s idle: %v", err)
	}
	t.Logf("the forward still carried traffic after the idle window, echoing %q", echoed)
	t.Logf("forward output:\n%s", indent(f.stop()))
}
