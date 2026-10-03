package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// testAgent builds an agent with its rings in a temporary directory. The struct is
// assembled directly because New writes to the sandbox's log directory, which only
// exists inside a sandbox.
func testAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	ring, err := OpenRing(dir, "db", 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	own, err := OpenRing(dir, "agent", 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{
		cfg: &Config{
			Slot: "db-1", Service: "db", Project: "p",
			OverlayIP: "10.88.1.10", ControlToken: "secret", DoorbellPort: 48080,
			ReachablePorts: []int{5432},
		},
		ring: ring, own: own,
		lastActive: time.Now(), startedAt: time.Now(),
	}
}

func TestTunnelTargetAuthorizesOnlyDeclaredPorts(t *testing.T) {
	a := testAgent(t)
	cases := []struct {
		name    string
		addr    string
		any     bool
		want    string
		wantErr string
	}{
		{name: "declared port on loopback", addr: "127.0.0.1:5432", want: "127.0.0.1:5432"},
		{name: "declared port on localhost", addr: "localhost:5432", want: "localhost:5432"},
		{name: "declared port on its own overlay address", addr: "10.88.1.10:5432", want: "10.88.1.10:5432"},
		{name: "undeclared port is refused", addr: "127.0.0.1:22", wantErr: "does not declare port 22"},
		{name: "another slot is refused", addr: "10.88.1.11:5432", wantErr: "not an address of slot db-1"},
		{name: "the hub is refused", addr: "10.88.1.1:51820", wantErr: "not an address of slot db-1"},
		{name: "a peer alias is refused", addr: "postgres:5432", wantErr: "not an address of slot db-1"},
		{name: "any allows another slot", addr: "10.88.1.11:5432", any: true, want: "10.88.1.11:5432"},
		{name: "any allows a peer alias", addr: "postgres:5432", any: true, want: "postgres:5432"},
		{name: "empty is refused", addr: "", wantErr: "addr is required"},
		{name: "a bare port is refused", addr: "5432", wantErr: "must be host:port"},
		{name: "a non-numeric port is refused", addr: "127.0.0.1:psql", wantErr: "not a port"},
		{name: "port zero is refused", addr: "127.0.0.1:0", wantErr: "not a port"},
		{name: "an out-of-range port is refused", addr: "127.0.0.1:70000", wantErr: "not a port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.tunnelTarget(tc.addr, tc.any)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("tunnelTarget(%q, %v) = %q, wanted an error about %q", tc.addr, tc.any, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error was %q, wanted it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("tunnelTarget(%q, %v): %v", tc.addr, tc.any, err)
			}
			if got != tc.want {
				t.Fatalf("tunnelTarget(%q, %v) = %q, wanted %q", tc.addr, tc.any, got, tc.want)
			}
		})
	}
}

// echoServer stands in for the application: it upper-cases what it receives, so a
// reply proves bytes crossed in both directions rather than being echoed locally.
func echoServer(t *testing.T) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						c.Write([]byte(strings.ToUpper(string(buf[:n]))))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { ln.Close() }
}

// serveControl runs the real control server over an httptest listener, so the test
// exercises the same handler, auth, and upgrade the deployed agent does.
func serveControl(t *testing.T, a *Agent) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(a.NewControlServer().mux)
	t.Cleanup(srv.Close)
	return srv
}

func dialTunnel(t *testing.T, srv *httptest.Server, token, addr string, any bool) (net.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/tunnel?addr=" + addr
	if any {
		u += "&any=1"
	}
	h := http.Header{}
	if token != "" {
		h.Set("X-SC-Token", token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, resp, err
	}
	conn.SetReadLimit(-1)
	return websocket.NetConn(context.Background(), conn, websocket.MessageBinary), resp, nil
}

func TestTunnelCarriesBytesBothWays(t *testing.T) {
	port, stop := echoServer(t)
	defer stop()
	a := testAgent(t)
	a.cfg.ReachablePorts = []int{port}
	srv := serveControl(t, a)

	conn, _, err := dialTunnel(t, srv, "secret", "127.0.0.1:"+strconv.Itoa(port), false)
	if err != nil {
		t.Fatalf("opening the tunnel: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("select 1")); err != nil {
		t.Fatalf("writing through the tunnel: %v", err)
	}
	buf := make([]byte, 8)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if got := string(buf); got != "SELECT 1" {
		t.Fatalf("read %q through the tunnel, wanted %q", got, "SELECT 1")
	}
}

// A forwarded connection carries result sets far larger than any control message,
// which is what makes the streaming net.Conn wrapper the right transport rather
// than whole-message reads.
func TestTunnelCarriesPayloadsLargerThanTheDefaultReadLimit(t *testing.T) {
	port, stop := echoServer(t)
	defer stop()
	a := testAgent(t)
	a.cfg.ReachablePorts = []int{port}
	srv := serveControl(t, a)

	conn, _, err := dialTunnel(t, srv, "secret", "127.0.0.1:"+strconv.Itoa(port), false)
	if err != nil {
		t.Fatalf("opening the tunnel: %v", err)
	}
	defer conn.Close()

	const size = 512 << 10 // comfortably past the 32KiB default
	payload := strings.Repeat("ab", size/2)
	go conn.Write([]byte(payload))
	got := make([]byte, size)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("reading %d bytes back: %v", size, err)
	}
	if string(got) != strings.ToUpper(payload) {
		t.Fatalf("payload of %d bytes came back altered", size)
	}
}

func TestTunnelRequiresTheControlToken(t *testing.T) {
	port, stop := echoServer(t)
	defer stop()
	a := testAgent(t)
	a.cfg.ReachablePorts = []int{port}
	srv := serveControl(t, a)

	for _, token := range []string{"", "wrong"} {
		_, resp, err := dialTunnel(t, srv, token, "127.0.0.1:"+strconv.Itoa(port), false)
		if err == nil {
			t.Fatalf("token %q opened a tunnel, which must require the control token", token)
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q was refused with %v, wanted 401", token, resp)
		}
	}
}

func TestTunnelReportsARefusedTargetAsAnError(t *testing.T) {
	a := testAgent(t)
	srv := serveControl(t, a)
	_, resp, err := dialTunnel(t, srv, "secret", "10.88.1.11:5432", false)
	if err == nil {
		t.Fatal("a tunnel to another slot opened without --any")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("refused with %v, wanted 400 so the CLI can print the reason", resp)
	}
}

// A closed port must come back as a status the CLI can print, rather than as a
// socket that opens and shuts and leaves the operator guessing.
func TestTunnelToAClosedPortFailsBeforeUpgrading(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	a := testAgent(t)
	a.cfg.ReachablePorts = []int{port}
	srv := serveControl(t, a)

	_, resp, err := dialTunnel(t, srv, "secret", "127.0.0.1:"+strconv.Itoa(port), false)
	if err == nil {
		t.Fatal("a tunnel opened to a port nothing is listening on")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("refused with %v, wanted 502", resp)
	}
}

func TestOpenTunnelHoldsTheSlotAwake(t *testing.T) {
	port, stop := echoServer(t)
	defer stop()
	a := testAgent(t)
	a.cfg.ReachablePorts = []int{port}
	// Idle long enough that only an open tunnel can keep it from sleeping.
	a.cfg.IdleSeconds = 1
	a.mu.Lock()
	a.lastActive = time.Now().Add(-time.Hour)
	a.mu.Unlock()
	srv := serveControl(t, a)

	if act := a.Snapshot(); !act.ShouldSleep {
		t.Fatal("an idle slot with no connections should want to sleep")
	}

	conn, _, err := dialTunnel(t, srv, "secret", "127.0.0.1:"+strconv.Itoa(port), false)
	if err != nil {
		t.Fatalf("opening the tunnel: %v", err)
	}
	// Round-trip a byte so the handler has registered the tunnel before asking.
	conn.Write([]byte("x"))
	io.ReadFull(conn, make([]byte, 1))

	act := a.Snapshot()
	if act.Tunnels != 1 {
		t.Fatalf("reported %d forwarded connections, wanted 1", act.Tunnels)
	}
	if act.ShouldSleep {
		t.Fatal("a slot with a forward open must not want to sleep, or the reaper pauses under a live session")
	}

	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for a.Tunnels() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := a.Tunnels(); n != 0 {
		t.Fatalf("%d forwarded connection(s) still counted after the client closed", n)
	}
}

func TestDrainClosesForwardedConnections(t *testing.T) {
	port, stop := echoServer(t)
	defer stop()
	a := testAgent(t)
	a.cfg.ReachablePorts = []int{port}
	srv := serveControl(t, a)

	conn, _, err := dialTunnel(t, srv, "secret", "127.0.0.1:"+strconv.Itoa(port), false)
	if err != nil {
		t.Fatalf("opening the tunnel: %v", err)
	}
	defer conn.Close()
	conn.Write([]byte("x"))
	io.ReadFull(conn, make([]byte, 1))
	if a.Tunnels() != 1 {
		t.Fatal("the tunnel was not registered")
	}

	// Sleeping with a forward open should hand the operator a close rather than a
	// socket that freezes with the VM.
	if closed := a.Drain(); closed != 1 {
		t.Fatalf("Drain closed %d connection(s), wanted 1", closed)
	}
	if n := a.Tunnels(); n != 0 {
		t.Fatalf("%d forwarded connection(s) survived a drain", n)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("the client could still read after a drain")
	}
}
