package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// App runs the application the image declares.
type App struct {
	agent    *Agent
	mu       sync.Mutex
	exit     *AppExit
	restarts int
}

// AppExit records how the application ended, so a service that died is visible
// rather than silently absent.
type AppExit struct {
	Exited   bool      `json:"exited"`
	Code     int       `json:"code"`
	Signal   string    `json:"signal,omitempty"`
	At       time.Time `json:"at,omitempty"`
	Duration float64   `json:"ran_seconds,omitempty"`
	Restarts int       `json:"restarts,omitempty"`
	// Running reports whether a replacement is up, so a crash loop is
	// distinguishable from a service that stopped for good.
	Running bool `json:"running"`
}

// Supervise runs the application and restarts it after a failure, with backoff.
//
// This is the behavior the CLI promises when it rejects Compose's restart: key.
// A clean exit is treated as completion rather than something to retry, which is
// what a one-shot job needs.
func (p *App) Supervise(ctx context.Context) {
	go func() {
		backoff := time.Second
		for {
			err := p.Run(ctx)
			if ctx.Err() != nil {
				return
			}
			p.mu.Lock()
			exited := p.exit
			p.mu.Unlock()
			if err == nil {
				p.agent.logf("application completed; not restarting a clean exit")
				return
			}
			p.mu.Lock()
			p.restarts++
			count := p.restarts
			if exited != nil {
				exited.Restarts = count
				exited.Running = false
			}
			p.mu.Unlock()
			p.agent.logf("restarting in %s (restart %d)", backoff, count)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
}

func NewApp(a *Agent) *App { return &App{agent: a} }

func (a *Agent) appExit() *AppExit {
	if a.app == nil {
		return nil
	}
	a.app.mu.Lock()
	defer a.app.mu.Unlock()
	return a.app.exit
}

// Run starts the application and streams its output into the ring.
//
// The command comes from the image's entrypoint and cmd unless Compose overrode
// it, and it is executed verbatim, so an image built around its own init works
// unchanged.
func (p *App) Run(ctx context.Context) error {
	cfg := p.agent.cfg
	started := time.Now()
	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = environ(cfg)
	// A new process group so a signal reaches the whole tree, not just the leader.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", strings.Join(cfg.Command, " "), err)
	}
	p.mu.Lock()
	p.exit = &AppExit{Running: true, Restarts: p.restarts}
	p.mu.Unlock()
	p.agent.logf("started: %s", strings.Join(cfg.Command, " "))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.capture("stdout", stdout) }()
	go func() { defer wg.Done(); p.capture("stderr", stderr) }()
	wg.Wait()

	err = cmd.Wait()
	exit := &AppExit{Exited: true, At: time.Now().UTC(), Duration: time.Since(started).Seconds(), Restarts: p.restarts}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit.Code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				exit.Signal = ws.Signal().String()
			}
		} else {
			exit.Code = -1
		}
	}
	p.mu.Lock()
	p.exit = exit
	p.mu.Unlock()
	p.agent.ring.Flush()
	p.agent.logf("application exited with code %d after %.1fs", exit.Code, exit.Duration)
	if exit.Code != 0 {
		return fmt.Errorf("application exited with code %d", exit.Code)
	}
	return nil
}

// capture writes each line into the ring. A slow or failed log path must never
// block the application, so a write error is dropped and counted rather than
// propagated back into the pipe.
func (p *App) capture(stream string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if err := p.agent.ring.Write(stream, sc.Text()); err != nil {
			fmt.Fprintln(os.Stderr, "scagent: dropping a log line: "+err.Error())
		}
	}
}

// environ merges the resolved environment with the facts a service needs about
// where it is running. The SIMPLECLOUD_ prefix is reserved, and the CLI rejects a
// Compose file that sets one, so code reading these can trust them.
func environ(cfg *Config) []string {
	env := map[string]string{}
	for k, v := range cfg.Env {
		env[k] = v
	}
	env["SIMPLECLOUD"] = "1"
	env["SIMPLECLOUD_PROJECT"] = cfg.Project
	env["SIMPLECLOUD_SERVICE"] = cfg.Service
	env["SIMPLECLOUD_SLOT"] = cfg.Slot
	env["SIMPLECLOUD_REPLICA_INDEX"] = fmt.Sprint(cfg.Ordinal)
	env["SIMPLECLOUD_OVERLAY_IP"] = cfg.OverlayIP
	env["SIMPLECLOUD_REGION"] = cfg.Region
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
