package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

// tunnelDialTimeout bounds the dial inside the sandbox. The application may still
// be starting when the first forwarded connection arrives after a wake, so this is
// long enough to cover a slow boot and short enough to report a closed port.
const tunnelDialTimeout = 15 * time.Second

// tunnel carries one forwarded connection: a WebSocket from the operator's machine
// spliced to a TCP connection the sandbox opens on its behalf.
//
// The operator's machine has no route into the overlay, so a forward reaches the
// agent over the same authenticated ingress path as the rest of the control API
// and lets the agent do the dialling. That also means the request itself rings the
// doorbell, so forwarding to a sleeping service wakes it.
func (s *ControlServer) tunnel(w http.ResponseWriter, r *http.Request) {
	a := s.agent
	allowAny := r.URL.Query().Get("any") == "1"
	target, err := a.tunnelTarget(r.URL.Query().Get("addr"), allowAny)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Dial before upgrading, so a refused connection arrives as an HTTP status the
	// CLI can print rather than as a socket that opens and immediately shuts.
	upstream, err := net.DialTimeout("tcp", target, tunnelDialTimeout)
	if err != nil {
		a.logf("tunnel to %s could not be dialled: %v", target, err)
		http.Error(w, fmt.Sprintf("dialing %s: %v", target, err), http.StatusBadGateway)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		upstream.Close()
		a.logf("tunnel to %s was not upgraded: %v", target, err)
		return
	}
	// NetConn lifts the per-message read limit, which matters here: this carries
	// application traffic rather than control messages.
	stream := websocket.NetConn(context.Background(), conn, websocket.MessageBinary)

	a.addTunnel(upstream)
	a.logf("tunnel open to %s", target)
	start := time.Now()

	// Either side finishing ends the pair. Closing both unblocks the copy still in
	// flight, which is what lets the handler return rather than leaking a goroutine.
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, stream); done <- struct{}{} }()
	go func() { io.Copy(stream, upstream); done <- struct{}{} }()
	<-done

	a.removeTunnel(upstream)
	upstream.Close()
	conn.Close(websocket.StatusNormalClosure, "")
	a.logf("tunnel to %s closed after %s", target, time.Since(start).Round(time.Millisecond))
}

// tunnelTarget resolves and authorizes the address a forward asks for.
//
// Without this check the endpoint is an open proxy into the overlay: one control
// token would reach any address the sandbox can, including other projects'
// services through their relay aliases. The default therefore admits only this
// slot's own declared ports on its own addresses, and widening it is explicit.
func (a *Agent) tunnelTarget(addr string, allowAny bool) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("addr is required, as host:port")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("addr must be host:port, not %q", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("%q is not a port", portStr)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if allowAny {
		return net.JoinHostPort(host, portStr), nil
	}
	if !a.ownAddress(host) {
		return "", fmt.Errorf("%s is not an address of slot %s, which --any allows", host, a.cfg.Slot)
	}
	for _, p := range a.cfg.ReachablePorts {
		if p == port {
			return net.JoinHostPort(host, portStr), nil
		}
	}
	return "", fmt.Errorf("service %s does not declare port %d, which --any allows", a.cfg.Service, port)
}

func (a *Agent) ownAddress(host string) bool {
	if host == "localhost" || (a.cfg.OverlayIP != "" && host == a.cfg.OverlayIP) {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *Agent) addTunnel(c net.Conn) {
	a.tunMu.Lock()
	defer a.tunMu.Unlock()
	if a.tuns == nil {
		a.tuns = map[net.Conn]struct{}{}
	}
	a.tuns[c] = struct{}{}
}

func (a *Agent) removeTunnel(c net.Conn) {
	a.tunMu.Lock()
	defer a.tunMu.Unlock()
	delete(a.tuns, c)
}

// Tunnels reports how many forwarded connections are open. A forward holds the
// slot awake: the operator is using it, and the reaper pausing underneath a live
// psql session would look like the database crashing.
func (a *Agent) Tunnels() int {
	a.tunMu.Lock()
	defer a.tunMu.Unlock()
	return len(a.tuns)
}

// CloseTunnels ends every forwarded connection, so a slot told to sleep hands its
// operator a clean close rather than a socket that freezes mid-query.
func (a *Agent) CloseTunnels() int {
	a.tunMu.Lock()
	conns := make([]net.Conn, 0, len(a.tuns))
	for c := range a.tuns {
		conns = append(conns, c)
	}
	a.tuns = map[net.Conn]struct{}{}
	a.tunMu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	return len(conns)
}
