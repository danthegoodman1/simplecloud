package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// ControlServer is the doorbell and the control API on one listener.
//
// A port token carries HTTP/1.1, which is all either needs, and Archil validates
// the token before traffic arrives — so the wake path is authenticated by the
// platform and the control endpoints add a token of their own.
type ControlServer struct {
	agent *Agent
	mux   *http.ServeMux
}

func (a *Agent) NewControlServer() *ControlServer {
	s := &ControlServer{agent: a, mux: http.NewServeMux()}

	// The doorbell. Reaching this at all means Archil accepted a valid port token,
	// and the sandbox is awake by the time the handler runs.
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "simplecloud %s awake\n", a.cfg.Slot)
	})

	s.mux.HandleFunc("/v1/activity", s.authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, a.Snapshot())
	}))

	// Drain closes outbound relay connections so peers can go idle once this slot
	// pauses. The reaper calls it immediately before pausing.
	s.mux.HandleFunc("/v1/drain", s.authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"closed": a.Drain()})
	}))

	s.mux.HandleFunc("/v1/logs", s.authed(func(w http.ResponseWriter, r *http.Request) {
		from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 {
			limit = 200
		}
		ring := a.ring
		if r.URL.Query().Get("source") == "agent" {
			ring = a.own
		}
		lines, dropped, lastSeq, err := ring.Read(from, limit)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{
			"lines": lines, "dropped": dropped, "segment": lastSeq,
			"exit": a.appExit(),
		})
	}))

	s.mux.HandleFunc("/v1/status", s.authed(func(w http.ResponseWriter, r *http.Request) {
		outbound := 0
		for _, rel := range a.relays {
			outbound += rel.Outbound()
		}
		writeJSON(w, map[string]any{
			"slot": a.cfg.Slot, "service": a.cfg.Service,
			"overlay_ip": a.cfg.OverlayIP, "relays": len(a.relays),
			"outbound_connections": outbound, "activity": a.Snapshot(),
			"app": a.appExit(),
		})
	}))
	return s
}

// authed requires the control token on everything but the doorbell, so a wake
// cannot read logs or trigger a drain.
func (s *ControlServer) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-SC-Token") != s.agent.cfg.ControlToken {
			http.Error(w, "control token required", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *ControlServer) Serve(ctx context.Context) error {
	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(s.agent.cfg.DoorbellPort))
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	s.agent.logf("doorbell and control API listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
