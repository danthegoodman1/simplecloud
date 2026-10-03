package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Relay accepts on a loopback alias and forwards to a peer over the overlay.
//
// A service name resolves to the alias, so the relay knows which service and port
// a connection wanted. That is what lets it wake a sleeping peer before
// forwarding, without the hub having to inspect traffic.
type Relay struct {
	agent *Agent
	peer  Peer
	port  int

	listener net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	waking   sync.Mutex
	lastWake time.Time
}

func NewRelay(a *Agent, peer Peer, port int) *Relay {
	return &Relay{agent: a, peer: peer, port: port, conns: map[net.Conn]struct{}{}}
}

func (r *Relay) Addr() string { return fmt.Sprintf("%s:%d", r.peer.LoopbackIP, r.port) }

func (r *Relay) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", r.Addr())
	if err != nil {
		return err
	}
	r.listener = ln
	go r.accept(ctx)
	return nil
}

func (r *Relay) accept(ctx context.Context) {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		go r.handle(ctx, conn)
	}
}

func (r *Relay) handle(ctx context.Context, client net.Conn) {
	defer client.Close()
	target := fmt.Sprintf("%s:%d", r.peer.OverlayIP, r.port)

	upstream, err := r.dialWithWake(ctx, target)
	if err != nil {
		r.agent.logf("relay %s -> %s failed: %v", r.Addr(), target, err)
		return
	}
	defer upstream.Close()

	r.track(upstream)
	defer r.untrack(upstream)

	// Bytes are copied without inspection, so any TCP protocol works.
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// dialWithWake reaches the peer, waking it if it is asleep.
//
// A sleeping peer has no listener, so the first dial is what detects that it needs
// waking. Both the dial and the doorbell are retried until the deadline: the
// caller has just woken up itself, so its first outbound request can fail, and
// giving up on one failure would reset a connection that would have worked a
// moment later.
func (r *Relay) dialWithWake(ctx context.Context, target string) (net.Conn, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	if conn, err := d.DialContext(ctx, "tcp", target); err == nil {
		return conn, nil
	}
	if r.peer.DoorbellURL == "" {
		return nil, fmt.Errorf("%s is unreachable and has no doorbell", r.peer.Slot)
	}

	// Long enough to cover a resume, a WireGuard re-handshake on both sides, and an
	// application that takes a moment to listen.
	deadline := time.Now().Add(3 * time.Minute)
	var lastRing time.Time
	var lastErr error
	for attempt := 0; ; attempt++ {
		if time.Since(lastRing) > 15*time.Second {
			if err := r.ringDoorbell(ctx); err != nil {
				lastErr = err
				r.agent.logf("waking %s: %v", r.peer.Slot, err)
			}
			lastRing = time.Now()
		}
		conn, err := d.DialContext(ctx, "tcp", target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s did not become reachable after waking: %w", r.peer.Slot, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ringDoorbell makes an authenticated request to the peer's Archil ingress, which
// resumes a paused sandbox. The token is required: an unauthenticated request is
// refused and leaves the sandbox paused, so this cannot be triggered by anyone who
// merely learns the hostname.
func (r *Relay) ringDoorbell(ctx context.Context) error {
	r.waking.Lock()
	defer r.waking.Unlock()
	// Several connections arriving at once should produce one wake, not many.
	if time.Since(r.lastWake) < 2*time.Second {
		return nil
	}
	r.lastWake = time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", r.peer.DoorbellURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Archil-Token", r.peer.DoorbellToken)
	client := &http.Client{Timeout: 90 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("waking %s: %w", r.peer.Slot, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("waking %s was refused: the doorbell token is invalid or expired", r.peer.Slot)
	}
	r.agent.logf("rang %s's doorbell, %s in %s", r.peer.Slot, resp.Status, time.Since(start).Round(10*time.Millisecond))
	return nil
}

func (r *Relay) track(c net.Conn) {
	r.mu.Lock()
	r.conns[c] = struct{}{}
	r.mu.Unlock()
}

func (r *Relay) untrack(c net.Conn) {
	r.mu.Lock()
	delete(r.conns, c)
	r.mu.Unlock()
}

// CloseConnections closes every outbound connection, which is what lets a peer
// observe a clean FIN and start its own idle timer.
func (r *Relay) CloseConnections() int {
	r.mu.Lock()
	conns := make([]net.Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.conns = map[net.Conn]struct{}{}
	r.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	return len(conns)
}

func (r *Relay) Outbound() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}
