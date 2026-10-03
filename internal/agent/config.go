// Package agent is the in-sandbox agent and the contract the CLI writes for it.
//
// The sandbox never runs the image's entrypoint — PID 1 is Archil's init and
// nothing else starts — so the agent starts the application. It also owns the
// overlay interface, service-name resolution, the relays, volume mounts, log
// capture, and activity accounting, none of which can come from the image.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ConfigPath is where the CLI uploads the config and the agent reads it.
const ConfigPath = "/opt/simplecloud/agent.json"

// Paths inside a sandbox.
const (
	BinaryPath = "/opt/simplecloud/scagent"
	LogDir     = "/var/log/simplecloud"
	StateDir   = "/opt/simplecloud"
)

// Config is everything the agent needs. It contains no Archil credential: the
// agent can wake its peers and serve its own doorbell, and nothing more.
type Config struct {
	Project   string `json:"project"`
	ProjectID string `json:"project_id"`
	Service   string `json:"service"`
	Slot      string `json:"slot"`
	Ordinal   int    `json:"ordinal"`
	Region    string `json:"region"`

	OverlayIP    string `json:"overlay_ip"`
	OverlayCIDR  string `json:"overlay_cidr"`
	WGPrivateKey string `json:"wg_private_key"`
	Hub          Hub    `json:"hub"`

	// DoorbellPort serves both the authenticated wake endpoint and the control API
	// the CLI uses for activity, logs, and drain. One listener covers both because
	// a port token carries HTTP/1.1, which is all either needs.
	DoorbellPort int    `json:"doorbell_port"`
	ControlToken string `json:"control_token"`

	Command []string          `json:"command"`
	Env     map[string]string `json:"env"`
	WorkDir string            `json:"workdir"`
	User    string            `json:"user"`

	ReachablePorts []int   `json:"reachable_ports"`
	Peers          []Peer  `json:"peers"`
	Mounts         []Mount `json:"mounts"`

	LogRingBytes    int64 `json:"log_ring_bytes"`
	IdleSeconds     int   `json:"idle_timeout_seconds"`
	KeepAwake       bool  `json:"keep_awake"`
	HeartbeatMillis int   `json:"heartbeat_millis"`
}

type Hub struct {
	PublicKey string `json:"public_key"`
	Endpoint  string `json:"endpoint"`
	OverlayIP string `json:"overlay_ip"`
}

// Peer is another service in the project, reachable by name through a local relay.
type Peer struct {
	Service    string   `json:"service"`
	Slot       string   `json:"slot"`
	Aliases    []string `json:"aliases"`
	OverlayIP  string   `json:"overlay_ip"`
	LoopbackIP string   `json:"loopback_ip"`
	Ports      []int    `json:"ports"`
	// DoorbellURL and DoorbellToken let this agent wake the peer before relaying.
	// They are what make a sleeping database reachable without a public port.
	DoorbellURL   string `json:"doorbell_url"`
	DoorbellToken string `json:"doorbell_token"`
}

type Mount struct {
	DiskID string `json:"disk_id"`
	Token  string `json:"token"`
	Path   string `json:"path"`
	Region string `json:"region"`
	// Sync marks a disk whose contents the CLI replaces from a local path on every
	// deploy, rather than one the service owns.
	Sync bool `json:"sync"`
}

func (c *Config) HeartbeatInterval() time.Duration {
	if c.HeartbeatMillis <= 0 {
		// A resumed sandbox does not re-handshake promptly on its own, and the hub
		// cannot provoke it because the sandbox is the side behind NAT. One second
		// turns a ~16s recovery into ~1.4s.
		return time.Second
	}
	return time.Duration(c.HeartbeatMillis) * time.Millisecond
}

func (c *Config) IdleTimeout() time.Duration { return time.Duration(c.IdleSeconds) * time.Second }

func (c *Config) Validate() error {
	switch {
	case c.Slot == "":
		return fmt.Errorf("agent config has no slot name")
	case c.OverlayIP == "":
		return fmt.Errorf("agent config has no overlay address")
	case c.WGPrivateKey == "":
		return fmt.Errorf("agent config has no WireGuard key")
	case c.Hub.PublicKey == "" || c.Hub.Endpoint == "":
		return fmt.Errorf("agent config has no hub peer")
	case len(c.Command) == 0:
		return fmt.Errorf("agent config has no command to run")
	case c.DoorbellPort == 0:
		return fmt.Errorf("agent config has no doorbell port")
	case c.ControlToken == "":
		return fmt.Errorf("agent config has no control token")
	}
	return nil
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) Marshal() ([]byte, error) { return json.MarshalIndent(c, "", "  ") }
