package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/state"
)

func lookupEnv(key string) (string, bool) { return os.LookupEnv(key) }

func psCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "ps",
		Short: "Show one row per slot: state, address, resources, activity, ports",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, done, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer done()
			var projects []*state.Project
			if all {
				if projects, err = e.store.ListProjects(); err != nil {
					return err
				}
			} else {
				_, proj, err := e.loadProject(false)
				if err != nil {
					return err
				}
				projects = []*state.Project{proj}
			}
			type row struct {
				Project  string `json:"project"`
				Slot     string `json:"slot"`
				State    string `json:"state"`
				Overlay  string `json:"overlay_ip"`
				VCPU     int    `json:"vcpu"`
				MemMiB   int    `json:"mem_mib"`
				Activity string `json:"activity"`
				Ports    string `json:"ports"`
			}
			var rows []row
			for _, proj := range projects {
				slots, err := e.store.ListSlots(proj.ID)
				if err != nil {
					return err
				}
				exposures, err := e.store.ListExposures(proj.ID)
				if err != nil {
					return err
				}
				published := map[string][]int{}
				for _, x := range exposures {
					published[x.Slot] = append(published[x.Slot], x.Port)
				}
				for _, s := range slots {
					live := s.SandboxStatus
					if s.SandboxID == "" {
						live = "not created"
					} else if sb, err := e.client.GetSandbox(e.ctx, s.SandboxID); err == nil {
						live = string(sb.Status)
						if live != s.SandboxStatus {
							s.SandboxStatus = live
							_ = e.store.PutSlot(s)
						}
					} else {
						live = "missing"
					}
					svc, _ := e.store.GetService(proj.ID, s.Service)
					vcpu, mem, idle, keep := serviceShape(svc)
					r := row{
						Project: proj.Name, Slot: s.Name, State: displayState(live),
						Overlay: s.OverlayIP, VCPU: vcpu, MemMiB: mem,
						Activity: activityCell(e, s, live, idle, keep),
						Ports:    portsCell(published[s.Name]),
					}
					rows = append(rows, r)
				}
			}
			if flags.JSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No slots yet. Run simplecloud up.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			header := "SLOT\tSTATE\tOVERLAY\tCPU/MEM\tACTIVITY\tPORTS"
			if all {
				header = "PROJECT\t" + header
			}
			fmt.Fprintln(tw, header)
			for _, r := range rows {
				line := fmt.Sprintf("%s\t%s\t%s\t%d / %d\t%s\t%s",
					r.Slot, r.State, r.Overlay, r.VCPU, r.MemMiB, r.Activity, r.Ports)
				if all {
					line = r.Project + "\t" + line
				}
				fmt.Fprintln(tw, line)
			}
			return tw.Flush()
		},
	}
	c.Flags().BoolVar(&all, "all", false, "span every project")
	return c
}

// displayState speaks the product's language rather than the platform's: a paused
// sandbox is asleep, which is the behavior the operator asked for.
func displayState(status string) string {
	switch status {
	case "paused":
		return "asleep"
	case "running":
		return "running"
	case "":
		return "not created"
	default:
		return status
	}
}

func serviceShape(svc *state.Service) (vcpu, mem int, idle time.Duration, keep bool) {
	vcpu, mem = 1, 1024
	if svc == nil {
		return
	}
	var spec struct {
		IdleSeconds int  `json:"idle_seconds"`
		KeepAwake   bool `json:"keep_awake"`
	}
	_ = json.Unmarshal([]byte(svc.Spec), &spec)
	return vcpu, mem, time.Duration(spec.IdleSeconds) * time.Second, spec.KeepAwake
}

// activityCell asks the agent only when the slot is awake. Asking a paused slot
// would wake it, which is a surprising way to spend a cold start.
func activityCell(e *env, s *state.Slot, live string, idle time.Duration, keep bool) string {
	switch {
	case keep:
		return "keep-awake"
	case live == "paused":
		return "asleep"
	case live != "running":
		return "-"
	}
	d, closeHub, err := e.deployerQuiet()
	if err != nil {
		return "?"
	}
	defer closeHub()
	act, err := d.AgentActivity(s)
	if err != nil {
		return "unreachable"
	}
	if act.Connections > 0 {
		return fmt.Sprintf("%d conn", act.Connections)
	}
	if idle > 0 {
		return fmt.Sprintf("idle %s", shortDuration(time.Duration(act.IdleFor)*time.Second))
	}
	return "idle"
}

func portsCell(ports []int) string {
	if len(ports) == 0 {
		return "-"
	}
	sort.Ints(ports)
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p) + " public"
	}
	return strings.Join(parts, ", ")
}

func endpointsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "endpoints",
		Short: "Show public URLs and in-project addresses together",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, done, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			_, proj, err := e.loadProject(false)
			if err != nil {
				return err
			}
			return e.printEndpoints(cmd, proj, false)
		},
	}
}

// printEndpoints answers reachability in one place, because the answer differs by
// caller: a public URL from outside, a service name from inside the project.
func (e *env) printEndpoints(cmd *cobra.Command, proj *state.Project, afterDeploy bool) error {
	exposures, err := e.store.ListExposures(proj.ID)
	if err != nil {
		return err
	}
	slots, err := e.store.ListSlots(proj.ID)
	if err != nil {
		return err
	}
	type entry struct {
		Service string `json:"service"`
		Port    int    `json:"port"`
		Scope   string `json:"scope"`
		Address string `json:"address"`
		Overlay string `json:"overlay_ip,omitempty"`
	}
	var entries []entry
	bySlot := map[string]*state.Slot{}
	for _, s := range slots {
		bySlot[s.Name] = s
	}
	for _, x := range exposures {
		svc := x.Slot
		if s, ok := bySlot[x.Slot]; ok {
			svc = s.Service
		}
		entries = append(entries, entry{Service: svc, Port: x.Port, Scope: "public", Address: "https://" + x.Hostname})
	}
	services, err := e.store.ListServices(proj.ID)
	if err != nil {
		return err
	}
	published := map[string]bool{}
	for _, x := range exposures {
		published[fmt.Sprintf("%s/%d", x.Slot, x.Port)] = true
	}
	for _, svc := range services {
		var spec struct {
			Reachable []int `json:"reachable"`
		}
		_ = json.Unmarshal([]byte(svc.Spec), &spec)
		slot := bySlot[svc.Name+"-1"]
		if slot == nil {
			continue
		}
		for _, port := range reachableFor(e, proj, svc.Name) {
			if published[fmt.Sprintf("%s-1/%d", svc.Name, port)] {
				continue
			}
			entries = append(entries, entry{
				Service: svc.Name, Port: port, Scope: "private",
				Address: fmt.Sprintf("%s:%d", svc.Name, port), Overlay: slot.OverlayIP,
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Service != entries[j].Service {
			return entries[i].Service < entries[j].Service
		}
		return entries[i].Port < entries[j].Port
	})
	if flags.JSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	out := cmd.OutOrStdout()
	if len(entries) == 0 {
		fmt.Fprintln(out, "No endpoints yet.")
		return nil
	}
	if afterDeploy {
		fmt.Fprintln(out, "endpoints")
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tPORT\tSCOPE\tADDRESS")
	for _, x := range entries {
		addr := x.Address
		if x.Overlay != "" {
			addr = fmt.Sprintf("%-24s (overlay %s)", x.Address, x.Overlay)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", x.Service, x.Port, x.Scope, addr)
	}
	return tw.Flush()
}

// reachableFor re-reads the Compose file so private ports are shown even for a
// service recorded before reachable ports were tracked.
func reachableFor(e *env, proj *state.Project, service string) []int {
	cp, _, err := e.loadProject(false)
	if err != nil {
		return nil
	}
	svc := cp.Service(service)
	if svc == nil {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, p := range svc.Ports {
		if !seen[p.Container] {
			seen[p.Container] = true
			out = append(out, p.Container)
		}
	}
	sort.Ints(out)
	return out
}

func urlCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "url <service>[:port]",
		Short: "Print one service's public URL, for scripting",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, done, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			_, proj, err := e.loadProject(false)
			if err != nil {
				return err
			}
			name, portSpec, hasPort := strings.Cut(args[0], ":")
			exposures, err := e.store.ListExposures(proj.ID)
			if err != nil {
				return err
			}
			slots, err := e.store.ListSlots(proj.ID)
			if err != nil {
				return err
			}
			service := map[string]string{}
			for _, s := range slots {
				service[s.Name] = s.Service
			}
			var matches []*state.Exposure
			for _, x := range exposures {
				if service[x.Slot] != name && x.Slot != name {
					continue
				}
				if hasPort {
					if want, err := strconv.Atoi(portSpec); err != nil || want != x.Port {
						continue
					}
				}
				matches = append(matches, x)
			}
			switch len(matches) {
			case 0:
				return fmt.Errorf("%s publishes no port.\n  Run simplecloud endpoints to see what is reachable", args[0])
			case 1:
				fmt.Fprintln(cmd.OutOrStdout(), "https://"+matches[0].Hostname)
				return nil
			default:
				// Guessing among several would break a script silently, so name them
				// and fail instead.
				var b strings.Builder
				fmt.Fprintf(&b, "%s publishes %d ports; name one:\n", name, len(matches))
				sort.Slice(matches, func(i, j int) bool { return matches[i].Port < matches[j].Port })
				for _, m := range matches {
					fmt.Fprintf(&b, "  simplecloud url %s:%d\n", name, m.Port)
				}
				return SilentError{fmt.Errorf("%s", b.String())}
			}
		},
	}
}
