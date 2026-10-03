// Package plan turns a Compose project into the concrete end state to converge on.
package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/danthegoodman1/simplecloud/internal/compose"
)

// ImageConfig is what an image already declares. Reading it is what makes
// `command` defaultable: a sandbox never runs the image's entrypoint, so without
// this every service would need an explicit command.
type ImageConfig struct {
	Reference   string
	Digest      string
	Entrypoint  []string
	Cmd         []string
	Env         map[string]string
	WorkingDir  string
	User        string
	Ports       []int
	Healthcheck []string
}

// Resolved is one service with every default filled in.
type Resolved struct {
	Service *compose.Service

	Image       string
	ImageDigest string
	ImageConfig *ImageConfig

	// Command is what the agent executes, taken from the image unless overridden.
	Command []string
	Env     map[string]string
	WorkDir string
	User    string

	// ReachablePorts is the union of the image's exposed ports and any ports:
	// entry, and is what the relays and the firewall are built from.
	ReachablePorts []int
	// PublishPorts are published to the internet. A datastore port is not here
	// unless the author opted in.
	PublishPorts []int
	SkippedPorts []compose.Port

	ConfigHash string
}

// Fetcher reads an image's configuration from a registry.
type Fetcher struct {
	Keychain authn.Keychain
}

func NewFetcher() *Fetcher {
	// DefaultKeychain reads the Docker credential store, so a registry the
	// operator has already logged into needs no separate configuration.
	return &Fetcher{Keychain: authn.DefaultKeychain}
}

func (f *Fetcher) Config(ctx context.Context, reference string) (*ImageConfig, error) {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return nil, fmt.Errorf("image reference %q: %w", reference, err)
	}
	// linux/amd64 explicitly: a multi-arch index would otherwise resolve to the
	// host's architecture, which is not what runs in a sandbox.
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(f.Keychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
	)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", reference, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("reading the config of %s: %w", reference, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, err
	}
	out := &ImageConfig{
		Reference:  reference,
		Digest:     digest.String(),
		Entrypoint: cfg.Config.Entrypoint,
		Cmd:        cfg.Config.Cmd,
		Env:        map[string]string{},
		WorkingDir: cfg.Config.WorkingDir,
		User:       cfg.Config.User,
	}
	for _, e := range cfg.Config.Env {
		if k, v, found := strings.Cut(e, "="); found {
			out.Env[k] = v
		}
	}
	for p := range cfg.Config.ExposedPorts {
		spec, proto, _ := strings.Cut(p, "/")
		if proto != "" && strings.ToLower(proto) != "tcp" {
			continue
		}
		if n, err := strconv.Atoi(spec); err == nil {
			out.Ports = append(out.Ports, n)
		}
	}
	sort.Ints(out.Ports)
	if hc := cfg.Config.Healthcheck; hc != nil {
		out.Healthcheck = hc.Test
	}
	return out, nil
}

// Resolve fills in every default for one service.
func Resolve(ctx context.Context, f *Fetcher, projectDir string, s *compose.Service, imageRef string) (*Resolved, error) {
	r := &Resolved{Service: s, Image: imageRef, Env: map[string]string{}}

	cfg, err := f.Config(ctx, imageRef)
	if err != nil {
		return nil, err
	}
	r.ImageConfig = cfg
	r.ImageDigest = cfg.Digest
	r.WorkDir = cfg.WorkingDir
	r.User = cfg.User

	// Environment resolution, lowest precedence first. Deploy-only keys sit above
	// the shared ones so a local endpoint can be replaced without a second file.
	for k, v := range cfg.Env {
		r.Env[k] = v
	}
	for _, file := range s.EnvFiles {
		vals, err := readEnvFile(filepath.Join(projectDir, file))
		if err != nil {
			return nil, err
		}
		for k, v := range vals {
			r.Env[k] = v
		}
	}
	for k, v := range s.Environment {
		r.Env[k] = v
	}
	if s.DeployEnvFile != "" {
		vals, err := readEnvFile(filepath.Join(projectDir, s.DeployEnvFile))
		if err != nil {
			return nil, err
		}
		for k, v := range vals {
			r.Env[k] = v
		}
	}
	for k, v := range s.DeployEnv {
		r.Env[k] = v
	}
	// A bare NAME in Compose takes its value from the shell.
	for k, v := range r.Env {
		if v == "" {
			if host, ok := os.LookupEnv(k); ok {
				r.Env[k] = host
			}
		}
	}
	for k := range r.Env {
		if strings.HasPrefix(k, "SIMPLECLOUD") {
			return nil, fmt.Errorf("%s: %s is reserved and set by the platform.\n  Remove it; code that reads it must be able to trust it", s.Name, k)
		}
	}

	switch {
	case len(s.Command) > 0:
		r.Command = s.Command
	default:
		r.Command = append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
	}
	if len(r.Command) == 0 {
		return nil, fmt.Errorf("%s: no command to run.\n  The image declares no entrypoint or cmd, so set command: in the Compose file", s.Name)
	}

	ports := map[int]bool{}
	for _, p := range cfg.Ports {
		ports[p] = true
	}
	for _, p := range s.Expose {
		ports[p] = true
	}
	for _, p := range s.Ports {
		ports[p.Container] = true
		if p.Skipped {
			r.SkippedPorts = append(r.SkippedPorts, p)
			continue
		}
		r.PublishPorts = append(r.PublishPorts, p.Container)
	}
	for p := range ports {
		r.ReachablePorts = append(r.ReachablePorts, p)
	}
	sort.Ints(r.ReachablePorts)
	sort.Ints(r.PublishPorts)

	r.ConfigHash = hashResolved(r)
	return r, nil
}

// hashResolved covers everything a sandbox is created with. A sandbox's
// environment is fixed at creation, so any change here means replacing it — and an
// unchanged hash is what lets `up` do nothing on an untouched project.
func hashResolved(r *Resolved) string {
	s := r.Service
	payload := struct {
		Digest    string             `json:"digest"`
		Command   []string           `json:"command"`
		Env       map[string]string  `json:"env"`
		WorkDir   string             `json:"workdir"`
		User      string             `json:"user"`
		Reachable []int              `json:"reachable"`
		Publish   []int              `json:"publish"`
		VCPU      int                `json:"vcpu"`
		MemMiB    int                `json:"mem"`
		MaxTTL    int                `json:"max_ttl"`
		Doorbell  int                `json:"doorbell"`
		Mounts    []compose.Mount    `json:"mounts"`
		Sync      []compose.SyncPath `json:"sync"`
		Egress    []string           `json:"egress"`
		LogRing   int64              `json:"log_ring"`
	}{
		Digest: r.ImageDigest, Command: r.Command, Env: r.Env, WorkDir: r.WorkDir,
		User: r.User, Reachable: r.ReachablePorts, Publish: r.PublishPorts,
		VCPU: s.VCPU, MemMiB: s.MemMiB, MaxTTL: s.MaxTTL, Doorbell: s.DoorbellPort,
		Mounts: s.Mounts, Sync: s.Sync, Egress: s.Egress, LogRing: s.LogRing,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic("plan: hashing resolved service: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// readEnvFile parses a KEY=VALUE file, ignoring comments and blank lines.
func readEnvFile(path string) (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading env file %s: %w", path, err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: want KEY=VALUE", path, i+1)
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, nil
}
