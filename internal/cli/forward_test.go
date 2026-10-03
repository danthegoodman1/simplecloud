package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseForwardAcceptsTheThreeSpellings(t *testing.T) {
	cases := []struct {
		arg  string
		want forwardSpec
	}{
		{"postgres", forwardSpec{Target: "postgres"}},
		{"postgres:5432", forwardSpec{Target: "postgres", Remote: 5432}},
		{"15432:postgres:5432", forwardSpec{Local: 15432, Target: "postgres", Remote: 5432}},
		// A slot name is as valid a target as a service name.
		{"postgres-1:5432", forwardSpec{Target: "postgres-1", Remote: 5432}},
		// The local and remote ports are independent, including when they match.
		{"5432:postgres:5432", forwardSpec{Local: 5432, Target: "postgres", Remote: 5432}},
		{"1:db:65535", forwardSpec{Local: 1, Target: "db", Remote: 65535}},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			got, err := parseForward(tc.arg)
			if err != nil {
				t.Fatalf("parseForward(%q): %v", tc.arg, err)
			}
			if got != tc.want {
				t.Fatalf("parseForward(%q) = %+v, wanted %+v", tc.arg, got, tc.want)
			}
		})
	}
}

func TestParseForwardRejectsWhatItCannotRead(t *testing.T) {
	for _, arg := range []string{
		"",                    // nothing
		":5432",               // no service
		"postgres:",           // no port
		"postgres:psql",       // a name where a port belongs
		"postgres:0",          // out of range low
		"postgres:65536",      // out of range high
		"postgres:-1",         // negative
		"15432::5432",         // no service in the three-part form
		"abc:postgres:5432",   // a name where the local port belongs
		"15432:postgres:psql", // a name where the remote port belongs
		"1:2:3:4",             // too many parts
	} {
		t.Run(arg, func(t *testing.T) {
			got, err := parseForward(arg)
			if err == nil {
				t.Fatalf("parseForward(%q) = %+v, wanted an error", arg, got)
			}
			// The error has to say what the accepted forms are, because the whole
			// argument is one token and there is nothing else to go on.
			if !strings.Contains(err.Error(), "LOCAL:service:PORT") {
				t.Fatalf("error %q should show the accepted forms", err)
			}
		})
	}
}

// upstreamEcho stands in for the agent's end of a tunnel: it upper-cases what it
// receives, so a reply proves bytes crossed rather than being echoed locally.
func upstreamEcho(t *testing.T) (func(context.Context) (net.Conn, error), func() int) {
	t.Helper()
	var mu sync.Mutex
	opened := 0
	dial := func(ctx context.Context) (net.Conn, error) {
		mu.Lock()
		opened++
		mu.Unlock()
		here, there := net.Pipe()
		go func() {
			defer there.Close()
			buf := make([]byte, 4096)
			for {
				n, err := there.Read(buf)
				if n > 0 {
					there.Write([]byte(strings.ToUpper(string(buf[:n]))))
				}
				if err != nil {
					return
				}
			}
		}()
		return here, nil
	}
	return dial, func() int {
		mu.Lock()
		defer mu.Unlock()
		return opened
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestServeForwardsCarriesLocalConnections covers the operator's half of a
// forward: the local listener, one tunnel per connection, and the shutdown that
// Ctrl-C triggers. The agent's half is covered in internal/agent.
func TestServeForwardsCarriesLocalConnections(t *testing.T) {
	dial, opened := upstreamEcho(t)
	port := freeLocalPort(t)
	f := &forward{Local: port, Addr: "127.0.0.1:5432", Label: "db-1:5432", dial: dial}

	ctx, cancel := context.WithCancel(context.Background())
	var out syncWriter
	stopped := make(chan error, 1)
	go func() { stopped <- serveForwards(ctx, []*forward{f}, "127.0.0.1", &out) }()

	// Every listener is bound before this line is printed, so waiting for it avoids
	// a probe connection, which would itself open a tunnel and be counted.
	waitOutput(t, &out, "Press Ctrl-C")
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Each connection should get its own tunnel, so three means three.
	for i := 0; i < 3; i++ {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatalf("connection %d to the forward: %v", i, err)
		}
		if _, err := c.Write([]byte("select 1")); err != nil {
			t.Fatalf("connection %d writing: %v", i, err)
		}
		buf := make([]byte, 8)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("connection %d reading back: %v", i, err)
		}
		if got := string(buf); got != "SELECT 1" {
			t.Fatalf("connection %d read %q, wanted %q", i, got, "SELECT 1")
		}
		c.Close()
	}
	if n := opened(); n != 3 {
		t.Fatalf("3 local connections opened %d tunnel(s)", n)
	}

	// Ctrl-C reaches the accept loop through the context.
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("serveForwards returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveForwards did not stop when the context was cancelled")
	}
	// The port has to be free again, or a second forward could not take it.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the local port was still held after shutdown: %v", err)
	}
	ln.Close()
	if !strings.Contains(out.String(), "stopped forwarding") {
		t.Fatalf("shutdown should be reported:\n%s", out.String())
	}
}

// A local port already in use has to be reported rather than silently dropped, and
// it must not leave the forwards that did bind holding their ports.
func TestServeForwardsReportsABusyLocalPort(t *testing.T) {
	dial, _ := upstreamEcho(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	taken := busy.Addr().(*net.TCPAddr).Port
	free := freeLocalPort(t)

	forwards := []*forward{
		{Local: free, Addr: "127.0.0.1:5432", Label: "db-1:5432", dial: dial},
		{Local: taken, Addr: "127.0.0.1:6379", Label: "redis-1:6379", dial: dial},
	}
	var out syncWriter
	err = serveForwards(context.Background(), forwards, "127.0.0.1", &out)
	if err == nil {
		t.Fatal("binding a port already in use should fail")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(taken)) {
		t.Fatalf("error %q should name the port it could not bind", err)
	}
	// The first listener must have been closed again, or retrying is impossible.
	ln, lerr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", free))
	if lerr != nil {
		t.Fatalf("a forward that did bind kept its port after the failure: %v", lerr)
	}
	ln.Close()
}

// A refused tunnel is per-connection: the listener stays up so the next attempt
// can succeed, which is what makes a forward survive a service restarting.
func TestForwardKeepsListeningAfterARefusedTunnel(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	echo, _ := upstreamEcho(t)
	dial := func(ctx context.Context) (net.Conn, error) {
		if fail.Load() {
			return nil, fmt.Errorf("db-1 refused a tunnel to 127.0.0.1:5432: nothing is listening")
		}
		return echo(ctx)
	}

	port := freeLocalPort(t)
	f := &forward{Local: port, Addr: "127.0.0.1:5432", Label: "db-1:5432", dial: dial}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncWriter
	go serveForwards(ctx, []*forward{f}, "127.0.0.1", &out)
	waitOutput(t, &out, "Press Ctrl-C")
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("first connection: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.ReadAll(c)
	c.Close()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "nothing is listening") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "nothing is listening") {
		t.Fatalf("the refusal should be reported to the operator:\n%s", out.String())
	}

	// The same forward must now work, without restarting the command.
	fail.Store(false)
	c2, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("the listener did not survive a refused tunnel: %v", err)
	}
	defer c2.Close()
	c2.Write([]byte("ok"))
	buf := make([]byte, 2)
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c2, buf); err != nil {
		t.Fatalf("reading after recovery: %v", err)
	}
	if string(buf) != "OK" {
		t.Fatalf("read %q after recovery, wanted %q", buf, "OK")
	}
}

func waitOutput(t *testing.T, w *syncWriter, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(w.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output never contained %q:\n%s", want, w.String())
}

type syncWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}
