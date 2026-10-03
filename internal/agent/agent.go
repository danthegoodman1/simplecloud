package agent

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// Agent runs inside a sandbox beside the application.
type Agent struct {
	cfg    *Config
	ring   *Ring
	own    *Ring
	relays []*Relay
	app    *App

	mu         sync.Mutex
	lastActive time.Time
	startedAt  time.Time
	drainedAt  time.Time
}

func New(cfg *Config) (*Agent, error) {
	appRing, err := OpenRing(LogDir, cfg.Service, cfg.LogRingBytes)
	if err != nil {
		return nil, err
	}
	// The agent's own log gets a smaller ring: it is where wake, tunnel, and mount
	// problems surface, so it has to survive, but it is never high volume.
	ownRing, err := OpenRing(LogDir, "agent", 1<<20)
	if err != nil {
		return nil, err
	}
	return &Agent{
		cfg: cfg, ring: appRing, own: ownRing,
		lastActive: time.Now(), startedAt: time.Now(),
	}, nil
}

func (a *Agent) Config() *Config { return a.cfg }
func (a *Agent) Ring() *Ring     { return a.ring }
func (a *Agent) OwnRing() *Ring  { return a.own }

func (a *Agent) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	_ = a.own.Write("agent", line)
	fmt.Fprintln(os.Stderr, "scagent: "+line)
}

// Run sets up the sandbox and supervises the application until the context ends.
func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(StateDir, 0o700); err != nil {
		return err
	}
	a.logf("starting slot %s of project %s", a.cfg.Slot, a.cfg.Project)
	a.prepareDevices()

	if err := a.SetupNetwork(ctx); err != nil {
		return fmt.Errorf("setting up the overlay: %w", err)
	}
	a.logf("overlay up on %s via hub %s", a.cfg.OverlayIP, a.cfg.Hub.Endpoint)

	go a.Heartbeat(ctx)
	go a.flushLoop(ctx)

	// Volumes must be mounted before the application starts, or it comes up with an
	// empty data directory and the failure is reported as a storage fault later.
	if err := a.MountVolumes(ctx); err != nil {
		return fmt.Errorf("mounting volumes: %w", err)
	}

	for _, peer := range a.cfg.Peers {
		for _, port := range peer.Ports {
			r := NewRelay(a, peer, port)
			if err := r.Start(ctx); err != nil {
				a.logf("relay for %s:%d not started: %v", peer.Service, port, err)
				continue
			}
			a.relays = append(a.relays, r)
		}
	}
	a.logf("%d relay(s) listening", len(a.relays))

	control := a.NewControlServer()
	go func() {
		if err := control.Serve(ctx); err != nil {
			a.logf("control server stopped: %v", err)
		}
	}()

	go a.activityLoop(ctx)

	a.app = NewApp(a)
	// The agent outlives the application deliberately. If it exited too, a crashed
	// service would take the doorbell with it and the slot would look unreachable
	// rather than broken — so logs, status, and the exit code would all be lost at
	// exactly the moment they are wanted.
	a.app.Supervise(ctx)
	<-ctx.Done()
	return nil
}

// flushLoop commits the log and drops the pages written, keeping the log off the
// pause snapshot.
func (a *Agent) flushLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.ring.Flush()
			a.own.Flush()
			return
		case <-t.C:
			a.ring.Flush()
			a.own.Flush()
		}
	}
}

// Drain closes every outbound relay connection so peers see a clean FIN.
//
// Pausing snapshots memory and stops the VM; it does not close sockets. Without
// this, a paused service's peers keep seeing ESTABLISHED and would not notice for
// as long as TCP keepalive takes, so a database would never go idle.
func (a *Agent) Drain() int {
	closed := 0
	for _, r := range a.relays {
		closed += r.CloseConnections()
	}
	a.mu.Lock()
	a.drainedAt = time.Now()
	a.mu.Unlock()
	a.logf("drained %d outbound relay connection(s)", closed)
	return closed
}
