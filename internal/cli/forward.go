package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/deploy"
	"github.com/danthegoodman1/simplecloud/internal/state"
)

// forwardSpec is one requested forward, before it is matched against the project.
type forwardSpec struct {
	// Local is the port to listen on, or 0 to reuse the remote port.
	Local int
	// Target is a service or slot name, or the address the via slot should dial.
	Target string
	// Remote is 0 when the spec names no port, meaning every port the service
	// declares.
	Remote int
}

// parseForward reads the three accepted spellings:
//
//	postgres              every port postgres declares, on the same local port
//	postgres:5432         that port, on the same local port
//	15432:postgres:5432   that port, on local 15432, as ssh -L spells it
func parseForward(arg string) (forwardSpec, error) {
	bad := func() (forwardSpec, error) {
		return forwardSpec{}, fmt.Errorf("%q is not a forward.\n  Use service, service:PORT, or LOCAL:service:PORT", arg)
	}
	parts := strings.Split(arg, ":")
	port := func(s string) (int, bool) {
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= 1 && n <= 65535
	}
	switch len(parts) {
	case 1:
		if parts[0] == "" {
			return bad()
		}
		return forwardSpec{Target: parts[0]}, nil
	case 2:
		remote, ok := port(parts[1])
		if !ok || parts[0] == "" {
			return bad()
		}
		return forwardSpec{Target: parts[0], Remote: remote}, nil
	case 3:
		local, okL := port(parts[0])
		remote, okR := port(parts[2])
		if !okL || !okR || parts[1] == "" {
			return bad()
		}
		return forwardSpec{Local: local, Target: parts[1], Remote: remote}, nil
	}
	return bad()
}

func containsInt(haystack []int, want int) bool {
	for _, n := range haystack {
		if n == want {
			return true
		}
	}
	return false
}

// forward is one running listener.
type forward struct {
	Local  int
	Addr   string // what the agent dials
	Label  string // what to show the operator
	Slot   *state.Slot
	Listen net.Listener
	// dial opens the upstream for one accepted connection. Each connection gets
	// its own, because a tunnel carries exactly one.
	dial func(context.Context) (net.Conn, error)
}

func forwardCmd() *cobra.Command {
	var (
		address  string
		allowAny bool
		via      string
	)
	c := &cobra.Command{
		Use:   "forward <service[:port] | local:service:port>...",
		Short: "Forward a service's port to a local port",
		Long: "forward carries a port from a deployed service to your machine, so local tools\n" +
			"reach it as though it ran here. The connection travels the agent's authenticated\n" +
			"control path rather than the private network, so nothing has to be configured on\n" +
			"this machine, and opening it wakes a sleeping service.",
		Example: "  simplecloud forward postgres\n" +
			"  simplecloud forward postgres:5432 redis:6379\n" +
			"  simplecloud forward 15432:postgres:5432\n" +
			"  simplecloud forward --via web --any 15432:postgres:5432",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			specs := make([]forwardSpec, 0, len(args))
			for _, a := range args {
				spec, err := parseForward(a)
				if err != nil {
					return err
				}
				specs = append(specs, spec)
			}

			e, done, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer done()
			_, proj, err := e.loadProject(false)
			if err != nil {
				return err
			}
			// Ctrl-C has to reach the accept loops rather than kill the process mid
			// connection, so the command owns its own signal context.
			ctx, stop := signal.NotifyContext(e.ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			e.ctx = ctx
			d := e.deployerAgentOnly()

			out := cmd.OutOrStdout()
			forwards, err := e.resolveForwards(d, proj, specs, via, allowAny, out)
			if err != nil {
				return err
			}
			if len(forwards) == 0 {
				return fmt.Errorf("nothing to forward")
			}
			return serveForwards(ctx, forwards, address, out)
		},
	}
	f := c.Flags()
	f.StringVar(&address, "address", "127.0.0.1", "local address to listen on")
	f.BoolVar(&allowAny, "any", false, "allow a target the service does not declare, such as another service through --via")
	f.StringVar(&via, "via", "", "forward through this service's agent, making it a jump host")
	return c
}

// resolveForwards turns specs into listeners-to-be, waking each slot it needs.
//
// Waking first is deliberate: the status call travels the doorbell, so by the time
// a local port is announced the service behind it is up, and the first connection
// is not the one that pays for the boot.
func (e *env) resolveForwards(d *deploy.Deployer, proj *state.Project, specs []forwardSpec, via string, allowAny bool, out io.Writer) ([]*forward, error) {
	var viaSlot *state.Slot
	if via != "" {
		slots, err := e.selectSlots(proj, []string{via})
		if err != nil {
			return nil, fmt.Errorf("--via %s: %w", via, err)
		}
		viaSlot = slots[0]
	}

	ports := map[string][]int{}
	var forwards []*forward
	for _, spec := range specs {
		slot := viaSlot
		label := spec.Target
		host := "127.0.0.1"
		if viaSlot != nil {
			// The target names an address in the via slot's view. A service name works
			// here because the agent resolves peers through its own relays, which is
			// what makes a jump host wake the service it reaches.
			host = spec.Target
			label = via + " → " + spec.Target
		} else {
			slots, err := e.selectSlots(proj, []string{spec.Target})
			if err != nil {
				return nil, err
			}
			slot = slots[0]
			label = slot.Name
		}

		if spec.Remote == 0 && viaSlot != nil {
			return nil, fmt.Errorf("%s needs a port, because --via cannot ask another service what it declares", spec.Target)
		}

		// Ask the slot which ports it declares, both to expand a spec that names
		// none and to refuse an impossible one before binding a local port. The
		// alternative is a forward that looks healthy until something connects, and
		// then reports the refusal in the wrong terminal.
		var known []int
		if viaSlot == nil && !allowAny {
			cached, ok := ports[slot.Name]
			if !ok {
				st, err := d.AgentStatus(slot)
				if err != nil {
					return nil, fmt.Errorf("asking %s which ports it declares: %w", slot.Name, err)
				}
				cached = st.ReachablePorts
				sort.Ints(cached)
				ports[slot.Name] = cached
			}
			known = cached
		}

		want := []int{spec.Remote}
		switch {
		case spec.Remote == 0:
			if len(known) == 0 {
				return nil, fmt.Errorf("%s declares no ports.\n  Name one explicitly, as %s:PORT", slot.Name, spec.Target)
			}
			want = known
		case known != nil && !containsInt(known, spec.Remote):
			return nil, fmt.Errorf("%s does not declare port %d, it declares %s.\n  Pass --any to forward it anyway",
				slot.Name, spec.Remote, joinInts(known))
		}

		for _, p := range want {
			local := spec.Local
			if local == 0 {
				local = p
			}
			f := &forward{
				Local: local,
				Addr:  net.JoinHostPort(host, strconv.Itoa(p)),
				Label: fmt.Sprintf("%s:%d", label, p),
				Slot:  slot,
			}
			f.dial = func(ctx context.Context) (net.Conn, error) {
				return d.AgentTunnel(ctx, f.Slot, f.Addr, allowAny)
			}
			forwards = append(forwards, f)
		}
	}
	return forwards, nil
}

func serveForwards(ctx context.Context, forwards []*forward, address string, out io.Writer) error {
	for _, f := range forwards {
		ln, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(f.Local)))
		if err != nil {
			for _, open := range forwards {
				if open.Listen != nil {
					open.Listen.Close()
				}
			}
			return fmt.Errorf("listening on %s:%d: %w", address, f.Local, err)
		}
		f.Listen = ln
	}

	fmt.Fprintln(out, "forwarding")
	for _, f := range forwards {
		fmt.Fprintf(out, "  %s:%-5d -> %s\n", address, f.Local, f.Label)
	}
	fmt.Fprintln(out, "Press Ctrl-C to stop.")

	var wg sync.WaitGroup
	for _, f := range forwards {
		wg.Add(1)
		go func(f *forward) {
			defer wg.Done()
			acceptForward(ctx, f, out)
		}(f)
	}
	<-ctx.Done()
	for _, f := range forwards {
		f.Listen.Close()
	}
	wg.Wait()
	fmt.Fprintln(out, "stopped forwarding")
	return nil
}

func acceptForward(ctx context.Context, f *forward, out io.Writer) {
	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		client, err := f.Listen.Accept()
		if err != nil {
			return
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			if err := pipeForward(ctx, f, client, out); err != nil {
				fmt.Fprintf(out, "  %s: %v\n", f.Label, err)
			}
		}()
	}
}

func pipeForward(ctx context.Context, f *forward, client net.Conn, out io.Writer) error {
	defer client.Close()
	start := time.Now()
	upstream, err := f.dial(ctx)
	if err != nil {
		return err
	}
	defer upstream.Close()
	fmt.Fprintf(out, "  %s connected (%s)\n", f.Label, time.Since(start).Round(time.Millisecond))
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}
