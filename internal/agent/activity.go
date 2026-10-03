package agent

import (
	"context"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"time"
)

// Activity is what the reaper asks for.
type Activity struct {
	Slot        string    `json:"slot"`
	Service     string    `json:"service"`
	Connections int       `json:"inbound_connections"`
	Tunnels     int       `json:"forwarded_connections"`
	LastActive  time.Time `json:"last_active"`
	IdleFor     float64   `json:"idle_seconds"`
	KeepAwake   bool      `json:"keep_awake"`
	IdleTimeout float64   `json:"idle_timeout_seconds"`
	ShouldSleep bool      `json:"should_sleep"`
	UptimeSecs  float64   `json:"uptime_seconds"`
	LogSegment  int64     `json:"log_segment"`
}

// Snapshot reports current activity.
//
// Activity is an established inbound connection on a declared port, or an open
// forwarded connection. Outbound connections are deliberately excluded: counting
// them would let a service holding an idle connection pool look busy forever and
// never sleep, and the cascade that lets a database sleep after its API does
// would never start.
func (a *Agent) Snapshot() Activity {
	conns := a.countInbound()
	// A forward to a port this service never declared, or through this slot to
	// another, does not appear in /proc/net/tcp as inbound, so open tunnels are
	// counted directly rather than inferred.
	tunnels := a.Tunnels()
	a.mu.Lock()
	if conns > 0 || tunnels > 0 {
		a.lastActive = time.Now()
	}
	last := a.lastActive
	started := a.startedAt
	a.mu.Unlock()

	idle := time.Since(last)
	act := Activity{
		Slot: a.cfg.Slot, Service: a.cfg.Service, Connections: conns, Tunnels: tunnels,
		LastActive: last.UTC(), IdleFor: idle.Seconds(),
		KeepAwake: a.cfg.KeepAwake, IdleTimeout: a.cfg.IdleTimeout().Seconds(),
		UptimeSecs: time.Since(started).Seconds(),
	}
	if a.ring != nil {
		act.LogSegment = a.ring.Dropped()
	}
	act.ShouldSleep = !a.cfg.KeepAwake && a.cfg.IdleSeconds > 0 &&
		conns == 0 && tunnels == 0 && idle >= a.cfg.IdleTimeout()
	return act
}

func (a *Agent) activityLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Snapshot()
		}
	}
}

// countInbound counts established TCP connections whose local port is one this
// service declared. /proc/net/tcp is read directly rather than shelling out, so
// the count does not depend on `ss` being present in the image.
func (a *Agent) countInbound() int {
	want := map[int]bool{}
	for _, p := range a.cfg.ReachablePorts {
		want[p] = true
	}
	if len(want) == 0 {
		return 0
	}
	total := 0
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if i == 0 {
				continue
			}
			f := strings.Fields(line)
			if len(f) < 4 {
				continue
			}
			// State 01 is ESTABLISHED. A listening socket is not activity.
			if f[3] != "01" {
				continue
			}
			local := f[1]
			colon := strings.LastIndex(local, ":")
			if colon < 0 {
				continue
			}
			port, err := strconv.ParseInt(local[colon+1:], 16, 32)
			if err != nil {
				continue
			}
			if !want[int(port)] {
				continue
			}
			// The doorbell and the control API are excluded by never being declared
			// reachable, so no filtering is needed here.
			total++
		}
	}
	return total
}

var _ = hex.DecodeString
