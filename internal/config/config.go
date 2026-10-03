// Package config resolves operator settings from flags, then environment, then
// an optional file. Environment alone is enough, so the CLI runs with no setup
// step: configuration says where the hub is, while state records what was done.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	APIKey      string
	Region      string
	Hub         string
	HubIdentity string
	HubEndpoint string
	Registry    string
	Home        string
	Profiles    []string
}

// Env var names. ARCHIL_API_KEY keeps its own name because it is that service's
// credential; everything belonging to Simplecloud carries the SIMPLECLOUD_ prefix.
const (
	EnvAPIKey      = "ARCHIL_API_KEY"
	EnvRegion      = "SIMPLECLOUD_REGION"
	EnvHub         = "SIMPLECLOUD_HUB"
	EnvHubIdentity = "SIMPLECLOUD_HUB_IDENTITY"
	EnvHubEndpoint = "SIMPLECLOUD_HUB_ENDPOINT"
	EnvRegistry    = "SIMPLECLOUD_REGISTRY"
	EnvHome        = "SIMPLECLOUD_HOME"
	EnvProfiles    = "SIMPLECLOUD_PROFILES"
)

// Flags are values given on the command line, which win over everything else.
type Flags struct {
	Region      string
	Hub         string
	HubIdentity string
	HubEndpoint string
	Registry    string
	Profiles    []string
}

// Load resolves configuration. Precedence is flags, then environment, then file.
func Load(f Flags) (*Config, error) {
	c := &Config{}
	home := os.Getenv(EnvHome)
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locating your home directory: %w", err)
		}
		home = filepath.Join(h, ".simplecloud")
	}
	c.Home = home

	file, err := loadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return nil, err
	}

	pick := func(flag, env, key string) string {
		if flag != "" {
			return flag
		}
		if v := os.Getenv(env); v != "" {
			return v
		}
		return file[key]
	}
	c.APIKey = pick("", EnvAPIKey, "api_key")
	c.Region = pick(f.Region, EnvRegion, "region")
	c.Hub = pick(f.Hub, EnvHub, "hub")
	c.HubIdentity = pick(f.HubIdentity, EnvHubIdentity, "hub_identity")
	c.HubEndpoint = pick(f.HubEndpoint, EnvHubEndpoint, "hub_endpoint")
	c.Registry = pick(f.Registry, EnvRegistry, "registry")

	switch {
	case len(f.Profiles) > 0:
		c.Profiles = f.Profiles
	case os.Getenv(EnvProfiles) != "":
		c.Profiles = splitList(os.Getenv(EnvProfiles))
	case file["profiles"] != "":
		c.Profiles = splitList(file["profiles"])
	}
	return c, nil
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *Config) StatePath() string { return filepath.Join(c.Home, "state.db") }
func (c *Config) LogsPath() string  { return filepath.Join(c.Home, "logs") }

// RequireAPIKey reports a missing credential with the fix rather than failing
// later inside an API call.
func (c *Config) RequireAPIKey() error {
	if c.APIKey == "" {
		return fmt.Errorf("no Archil API key.\n  Set %s, or add api_key to %s",
			EnvAPIKey, filepath.Join(c.Home, "config.toml"))
	}
	return nil
}

// RequireHub reports a missing hub with the fix. The hub's address is the one
// piece of infrastructure the CLI cannot invent.
func (c *Config) RequireHub() error {
	if c.Hub == "" {
		return fmt.Errorf("no WireGuard hub configured.\n  Set %s to an SSH target such as root@203.0.113.10,\n  or run: simplecloud hub add root@<address> --bootstrap", EnvHub)
	}
	return nil
}

// Endpoint is the address sandboxes dial, which defaults to the SSH target's host
// and differs only when the control path and data path are not the same.
func (c *Config) Endpoint() string {
	if c.HubEndpoint != "" {
		return c.HubEndpoint
	}
	_, host, _ := splitSSHTarget(c.Hub)
	return host
}

func splitSSHTarget(target string) (user, host string, port string) {
	port = "22"
	user = "root"
	rest := target
	if at := strings.LastIndex(target, "@"); at >= 0 {
		user, rest = target[:at], target[at+1:]
	}
	if colon := strings.LastIndex(rest, ":"); colon >= 0 && !strings.Contains(rest[colon:], "]") {
		rest, port = rest[:colon], rest[colon+1:]
	}
	return user, strings.Trim(rest, "[]"), port
}

// SSHTarget splits the configured hub into its parts.
func (c *Config) SSHTarget() (user, host, port string) { return splitSSHTarget(c.Hub) }

// loadFile reads a deliberately small key = value file. A full TOML parser would
// be a dependency for a handful of scalars.
func loadFile(path string) (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: want key = value", path, i+1)
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, nil
}

// Save writes the settings worth persisting, so later runs need no environment.
func (c *Config) Save() error {
	if err := os.MkdirAll(c.Home, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Written by simplecloud. Environment variables override these.\n")
	for _, kv := range [][2]string{
		{"region", c.Region}, {"hub", c.Hub}, {"hub_identity", c.HubIdentity},
		{"hub_endpoint", c.HubEndpoint}, {"registry", c.Registry},
	} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s = %q\n", kv[0], kv[1])
		}
	}
	return os.WriteFile(filepath.Join(c.Home, "config.toml"), []byte(b.String()), 0o600)
}
