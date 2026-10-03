package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danthegoodman1/simplecloud/internal/agent"
	"github.com/danthegoodman1/simplecloud/internal/agentbin"
	"github.com/danthegoodman1/simplecloud/internal/compose"
	"github.com/danthegoodman1/simplecloud/internal/plan"
	"github.com/danthegoodman1/simplecloud/internal/state"
)

func (d *Deployer) recordService(proj *state.Project, sp *plan.ServicePlan) error {
	svc := sp.Resolved.Service
	spec, err := json.Marshal(map[string]any{
		"idle_seconds": int(svc.IdleTimeout.Seconds()),
		"keep_awake":   svc.KeepAwake,
		"doorbell":     svc.DoorbellPort,
		"log_ring":     svc.LogRing,
		"replicas":     svc.Replicas,
		"profiles":     svc.Profiles,
	})
	if err != nil {
		return err
	}
	return d.Store.PutService(&state.Service{
		ProjectID: proj.ID, Name: svc.Name, Image: sp.Resolved.Image,
		ImageDigest: sp.Resolved.ImageDigest, ConfigHash: sp.Resolved.ConfigHash,
		Replicas: svc.Replicas, Spec: string(spec),
	})
}

// startAgent uploads the agent and its configuration, then runs it as a supervised
// process so it returns after a cold boot without the CLI being present.
func (d *Deployer) startAgent(cp *compose.Project, proj *state.Project, pl *plan.Plan, svc *compose.Service, slot *state.Slot) error {
	doneStep := d.step("%s installing agent", slot.Name)
	bin, err := agentbin.Bytes()
	if err != nil {
		return err
	}
	if err := d.Client.UploadFile(d.Ctx, slot.SandboxID, agent.BinaryPath, "0755", bytes.NewReader(bin)); err != nil {
		return fmt.Errorf("uploading the agent: %w", err)
	}
	doneStep("%d KiB", len(bin)/1024)

	cfg, err := d.agentConfig(cp, proj, pl, svc, slot)
	if err != nil {
		return err
	}
	raw, err := cfg.Marshal()
	if err != nil {
		return err
	}
	doneStep = d.step("%s writing configuration", slot.Name)
	if err := d.Client.UploadFile(d.Ctx, slot.SandboxID, agent.ConfigPath, "0600", bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("uploading the agent configuration: %w", err)
	}
	doneStep("%d peer(s), %d mount(s)", len(cfg.Peers), len(cfg.Mounts))

	if err := d.syncLocalPaths(svc, slot, cp.Dir); err != nil {
		return err
	}

	doneStep = d.step("%s starting agent", slot.Name)
	// Register the agent as a supervised Archil service. A service restarts on
	// exit, which is the only thing that brings the agent back after a cold boot
	// without the CLI being present — and a cold boot is what preemption, host
	// failure, and a pause that does not complete all leave behind.
	//
	// The doorbell stays token-only, so no --tcp-port here.
	supervised := false
	create := fmt.Sprintf("archil services delete %s >/dev/null 2>&1; archil services create %s -- %s",
		agentServiceName, agentServiceName, agent.BinaryPath)
	if _, err := d.Client.ExecOK(d.Ctx, slot.SandboxID, create); err != nil {
		d.note("%s: could not register the agent as a supervised service: %v", slot.Name, err)
	} else if out, err := d.Client.ExecOK(d.Ctx, slot.SandboxID, "archil services list"); err == nil &&
		strings.Contains(out, agentServiceName) {
		supervised = true
	}

	if supervised {
		slot.AgentProcessID = ""
		if err := d.Store.PutSlot(slot); err != nil {
			return err
		}
		doneStep("supervised, so it returns after a cold boot")
		return nil
	}

	// Without supervision the agent still runs, but a cold boot leaves the slot
	// unreachable until `up` or `reconcile` reinstalls it. Say so rather than
	// leaving it to be discovered by a failing request.
	d.note("%s: the agent is not supervised, so a cold boot will need simplecloud up", slot.Name)
	p, err := d.Client.Run(d.Ctx, slot.SandboxID, agent.BinaryPath, archilRunOptions(svc))
	if err != nil {
		return fmt.Errorf("starting the agent: %w", err)
	}
	slot.AgentProcessID = p.ID()
	p.Disconnect()
	if err := d.Store.PutSlot(slot); err != nil {
		return err
	}
	doneStep("process %s, unsupervised", shortID(p.ID()))
	return nil
}

// agentServiceName is the supervised service the agent runs as.
const agentServiceName = "simplecloud-agent"

// syncLocalPaths copies a declared local directory into its disk. The sync is
// one-way and clobbering, which suits configuration and assets and must never
// point at a path the service writes to.
func (d *Deployer) syncLocalPaths(svc *compose.Service, slot *state.Slot, projectDir string) error {
	for _, sp := range svc.Sync {
		local := sp.Local
		if !filepath.IsAbs(local) {
			local = filepath.Join(projectDir, local)
		}
		st, err := os.Stat(local)
		if err != nil {
			return fmt.Errorf("x-simplecloud-sync: %s: %w", sp.Local, err)
		}
		doneStep := d.step("%s syncing %s", slot.Name, sp.Local)
		var files int
		var bytesSent int64
		walk := func(path string, info os.FileInfo) error {
			rel, err := filepath.Rel(local, path)
			if err != nil {
				return err
			}
			target := filepath.ToSlash(filepath.Join(sp.Path, rel))
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			mode := fmt.Sprintf("%04o", info.Mode().Perm())
			if err := d.Client.UploadFile(d.Ctx, slot.SandboxID, target, mode, f); err != nil {
				return err
			}
			files++
			bytesSent += info.Size()
			return nil
		}
		if st.IsDir() {
			err = filepath.Walk(local, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					return nil
				}
				return walk(path, info)
			})
		} else {
			err = walk(local, st)
		}
		if err != nil {
			return fmt.Errorf("syncing %s: %w", sp.Local, err)
		}
		doneStep("%d file(s), %d KiB (local wins)", files, bytesSent/1024)
	}
	return nil
}

func archilRunOptions(svc *compose.Service) archilOpts {
	// The agent must outlive the deploy, so its process timeout is the sandbox's own
	// maximum rather than the deploy's.
	return archilOpts{TimeoutSeconds: svc.MaxTTL}
}

// agentConfig assembles what one slot needs, including every peer's doorbell so
// the relay can wake a sleeping service before forwarding to it.
func (d *Deployer) agentConfig(cp *compose.Project, proj *state.Project, pl *plan.Plan, svc *compose.Service, slot *state.Slot) (*agentConfig, error) {
	res := resolvedFor(pl, svc.Name)
	if res == nil {
		return nil, fmt.Errorf("no resolved configuration for %s", svc.Name)
	}
	cfg := &agentConfig{Config: agent.Config{
		Project: proj.Name, ProjectID: proj.ID, Service: svc.Name, Slot: slot.Name,
		Ordinal: slot.Ordinal, Region: proj.Region,
		OverlayIP: slot.OverlayIP, OverlayCIDR: proj.OverlayCIDR,
		WGPrivateKey: slot.WGPrivateKey,
		Hub: agent.Hub{
			PublicKey: proj.HubPublicKey,
			Endpoint:  fmt.Sprintf("%s:%d", pl.Hub.Endpoint, proj.HubListenPort),
			OverlayIP: proj.HubOverlayIP(),
		},
		DoorbellPort:   slot.DoorbellPort,
		ControlToken:   controlTokenFor(slot),
		Command:        res.Command,
		Env:            res.Env,
		WorkDir:        res.WorkDir,
		User:           res.User,
		ReachablePorts: res.ReachablePorts,
		LogRingBytes:   svc.LogRing,
		IdleSeconds:    int(svc.IdleTimeout.Seconds()),
		KeepAwake:      svc.KeepAwake,
	}, projectDir: cp.Dir}

	// Peers: every other slot in the project, reachable by name through a relay.
	slots, err := d.Store.ListSlots(proj.ID)
	if err != nil {
		return nil, err
	}
	for _, other := range slots {
		if other.Name == slot.Name {
			continue
		}
		otherRes := resolvedFor(pl, other.Service)
		if otherRes == nil {
			continue
		}
		aliases := []string{other.Name}
		// The base service name resolves to slot 1; there is no per-connection
		// balancing in this PoC.
		if other.Ordinal == 1 {
			aliases = append(aliases, other.Service)
		}
		sort.Strings(aliases)
		peer := agent.Peer{
			Service: other.Service, Slot: other.Name, Aliases: aliases,
			OverlayIP: other.OverlayIP, LoopbackIP: other.LoopbackIP,
			Ports: otherRes.ReachablePorts,
		}
		if other.DoorbellHost != "" {
			peer.DoorbellURL = "https://" + other.DoorbellHost + "/"
			peer.DoorbellToken = other.DoorbellToken
		}
		cfg.Peers = append(cfg.Peers, peer)
	}
	sort.Slice(cfg.Peers, func(i, j int) bool { return cfg.Peers[i].Slot < cfg.Peers[j].Slot })

	// Mounts: a volume belongs to one slot and is never shared between replicas.
	vols, err := d.Store.ListVolumes(proj.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		if v.OwnerSlot != slot.Name {
			continue
		}
		cfg.Mounts = append(cfg.Mounts, agent.Mount{
			DiskID: v.DiskID, Token: v.MountToken, Path: v.MountPath,
			Region: proj.Region, Sync: v.Kind == "sync",
		})
	}
	return cfg, nil
}

// agentConfig carries the project directory alongside the config so a sync source
// can be resolved without threading it separately.
type agentConfig struct {
	agent.Config
	projectDir string
}

type archilOpts = archilRunOpts

func resolvedFor(pl *plan.Plan, service string) *plan.Resolved {
	for _, sp := range pl.Services {
		if sp.Name == service {
			return sp.Resolved
		}
	}
	return nil
}

// controlTokenFor derives a stable per-slot control token from the slot's own
// WireGuard key, so it survives a sandbox replacement without being stored twice.
func controlTokenFor(slot *state.Slot) string {
	return sha256Sum(slot.WGPrivateKey + ":" + slot.Name)[:40]
}

// waitReady polls the agent's control API through the doorbell until the slot
// reports itself up, so readiness means reachable rather than merely created.
func (d *Deployer) waitReady(slot *state.Slot, timeout time.Duration) error {
	doneStep := d.step("%s waiting for ready", slot.Name)
	deadline := time.Now().Add(timeout)
	var last error
	for {
		st, err := d.AgentStatus(slot)
		if err == nil {
			// The agent answering is not enough: it outlives a crashed application,
			// so readiness has to mean the application itself is up. Otherwise a
			// crash loop reports as ready.
			if st.App != nil && st.App.Exited && !st.App.Running {
				last = fmt.Errorf("the application exited with code %d after %.1fs.\n"+
					"  Run simplecloud logs %s to see why", st.App.Code, st.App.Duration, slot.Name)
			} else {
				doneStep("overlay %s, %d relay(s)", slot.OverlayIP, st.Relays)
				return nil
			}
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("did not become ready within %s: %w", timeout, last)
		}
		time.Sleep(2 * time.Second)
	}
}

// AgentStatus is what the agent reports about itself.
type AgentStatus struct {
	Slot                string         `json:"slot"`
	Service             string         `json:"service"`
	OverlayIP           string         `json:"overlay_ip"`
	Relays              int            `json:"relays"`
	OutboundConnections int            `json:"outbound_connections"`
	Activity            agent.Activity `json:"activity"`
	App                 *agent.AppExit `json:"app"`
}

func (d *Deployer) AgentStatus(slot *state.Slot) (*AgentStatus, error) {
	var out AgentStatus
	if err := d.agentGet(slot, "/v1/status", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (d *Deployer) AgentActivity(slot *state.Slot) (*agent.Activity, error) {
	var out agent.Activity
	if err := d.agentGet(slot, "/v1/activity", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AgentDrain closes a slot's outbound relay connections so its peers observe a
// clean FIN and can start their own idle timers. Pausing alone freezes sockets
// rather than closing them, so without this a database would never go idle.
func (d *Deployer) AgentDrain(slot *state.Slot) (int, error) {
	var out struct {
		Closed int `json:"closed"`
	}
	if err := d.agentPost(slot, "/v1/drain", &out); err != nil {
		return 0, err
	}
	return out.Closed, nil
}

type LogChunk struct {
	Lines   []agent.Line   `json:"lines"`
	Dropped bool           `json:"dropped"`
	Segment int64          `json:"segment"`
	Exit    *agent.AppExit `json:"exit"`
}

func (d *Deployer) AgentLogs(slot *state.Slot, from int64, limit int, source string) (*LogChunk, error) {
	path := fmt.Sprintf("/v1/logs?from=%d&limit=%d", from, limit)
	if source != "" {
		path += "&source=" + source
	}
	var out LogChunk
	if err := d.agentGet(slot, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (d *Deployer) agentGet(slot *state.Slot, path string, out any) error {
	return d.agentDo("GET", slot, path, out)
}

func (d *Deployer) agentPost(slot *state.Slot, path string, out any) error {
	return d.agentDo("POST", slot, path, out)
}

// agentDo reaches the agent through its Archil port token. The request is
// authenticated twice: Archil checks the port token, and the agent checks the
// control token, so a wake cannot read logs or trigger a drain.
func (d *Deployer) agentDo(method string, slot *state.Slot, path string, out any) error {
	if slot.DoorbellHost == "" {
		return fmt.Errorf("slot %s has no doorbell yet", slot.Name)
	}
	url := "https://" + slot.DoorbellHost + path
	req, err := http.NewRequestWithContext(d.Ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Archil-Token", slot.DoorbellToken)
	req.Header.Set("X-SC-Token", controlTokenFor(slot))
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return fmt.Errorf("the doorbell token for %s was refused; it may have expired. Run simplecloud up to refresh it", slot.Name)
	default:
		return fmt.Errorf("agent on %s returned %s: %s", slot.Name, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "process_")
	if len(id) > 10 {
		return id[:10]
	}
	return id
}
