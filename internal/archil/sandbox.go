package archil

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

type SandboxStatus string

const (
	StatusPending  SandboxStatus = "pending"
	StatusRunning  SandboxStatus = "running"
	StatusPausing  SandboxStatus = "pausing"
	StatusPaused   SandboxStatus = "paused"
	StatusStopping SandboxStatus = "stopping"
	StatusStopped  SandboxStatus = "stopped"
	StatusExited   SandboxStatus = "exited"
	StatusFailed   SandboxStatus = "failed"
)

type Endpoint struct {
	Port     int    `json:"port"`
	Hostname string `json:"hostname"`
}

type Sandbox struct {
	ID             string        `json:"sandbox_id"`
	Name           string        `json:"name"`
	Status         SandboxStatus `json:"status"`
	VCPUCount      int           `json:"vcpu_count"`
	MemSizeMiB     int           `json:"mem_size_mib"`
	BaseImage      string        `json:"base_image"`
	ImageDigest    string        `json:"image_digest"`
	Platform       string        `json:"platform"`
	Endpoints      []Endpoint    `json:"endpoints"`
	MaxTTLSeconds  int           `json:"max_ttl_seconds"`
	IdleTTLSeconds int           `json:"idle_ttl_seconds"`
	ExitReason     string        `json:"exit_reason"`
	CreatedAt      time.Time     `json:"created_at"`
	LastActiveAt   time.Time     `json:"last_active_at"`
}

type CreateSandboxRequest struct {
	Name           string            `json:"name,omitempty"`
	BaseImage      string            `json:"base_image,omitempty"`
	ImageID        string            `json:"image_id,omitempty"`
	VCPUCount      int               `json:"vcpu_count,omitempty"`
	MemSizeMiB     int               `json:"mem_size_mib,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Ports          []int             `json:"ports,omitempty"`
	MaxTTLSeconds  int               `json:"max_ttl_seconds,omitempty"`
	IdleTTLSeconds *int              `json:"idle_ttl_seconds,omitempty"`
}

// CreateSandbox provisions a sandbox.
//
// Note: the API accepts a `network` field and silently ignores it, so egress has
// to be enabled with UpdateNetwork once the sandbox is running. Passing it here
// would look like it worked and leave egress denied.
func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest, wait bool) (*Sandbox, error) {
	q := url.Values{"wait": {strconv.FormatBool(wait)}}
	var sb Sandbox
	if err := c.do(ctx, "POST", "/api/sandboxes", q, req, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

func (c *Client) ListSandboxes(ctx context.Context) ([]Sandbox, error) {
	var out struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	if err := c.do(ctx, "GET", "/api/sandboxes", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Sandboxes, nil
}

func (c *Client) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	var sb Sandbox
	if err := c.do(ctx, "GET", "/api/sandboxes/"+id, nil, nil, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/sandboxes/"+id, nil, nil, nil)
}

func (c *Client) lifecycle(ctx context.Context, id, action string, wait bool) (*Sandbox, error) {
	q := url.Values{"wait": {strconv.FormatBool(wait)}}
	var sb Sandbox
	if err := c.do(ctx, "POST", "/api/sandboxes/"+id+"/"+action, q, nil, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// settle runs a lifecycle action and then polls until the sandbox leaves every
// transitional status. The API can return while still pausing or stopping, and a
// caller that acts on that intermediate state gets a 409 on its next request.
func (c *Client) settle(ctx context.Context, id, action string) (*Sandbox, error) {
	sb, err := c.lifecycle(ctx, id, action, true)
	if err != nil {
		return nil, err
	}
	switch sb.Status {
	case StatusPending, StatusPausing, StatusStopping:
		return c.WaitForStatus(ctx, id, 5*time.Minute)
	}
	return sb, nil
}

func (c *Client) StartSandbox(ctx context.Context, id string) (*Sandbox, error) {
	return c.settle(ctx, id, "start")
}

func (c *Client) StopSandbox(ctx context.Context, id string) (*Sandbox, error) {
	return c.settle(ctx, id, "stop")
}

// PauseSandbox snapshots memory and disk. Resume restores running processes,
// which is what makes a wake fast enough to hide behind one connection.
func (c *Client) PauseSandbox(ctx context.Context, id string) (*Sandbox, error) {
	return c.settle(ctx, id, "pause")
}

func (c *Client) ResumeSandbox(ctx context.Context, id string) (*Sandbox, error) {
	return c.settle(ctx, id, "resume")
}

// WaitForStatus polls until the sandbox leaves every transitional status.
func (c *Client) WaitForStatus(ctx context.Context, id string, timeout time.Duration) (*Sandbox, error) {
	deadline := time.Now().Add(timeout)
	for {
		sb, err := c.GetSandbox(ctx, id)
		if err != nil {
			return nil, err
		}
		switch sb.Status {
		case StatusPending, StatusPausing, StatusStopping:
		default:
			return sb, nil
		}
		if time.Now().After(deadline) {
			return sb, fmt.Errorf("archil: sandbox %s still %s after %s", id, sb.Status, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

type SetTimeoutRequest struct {
	Timeout        *int `json:"timeout,omitempty"`
	IdleTTLSeconds *int `json:"idle_ttl_seconds,omitempty"`
}

func (c *Client) SetTimeout(ctx context.Context, id string, req SetTimeoutRequest) error {
	return c.do(ctx, "POST", "/api/sandboxes/"+id+"/timeout", nil, req, nil)
}

// ---- ports ----

func (c *Client) ListPorts(ctx context.Context, id string) ([]Endpoint, error) {
	var out struct {
		Ports []Endpoint `json:"ports"`
	}
	if err := c.do(ctx, "GET", "/api/sandboxes/"+id+"/ports", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Ports, nil
}

// ExposePort publishes a TCP port. The resulting hostname is public and
// unauthenticated, accepting any TCP over TLS with SNI.
func (c *Client) ExposePort(ctx context.Context, id string, port int) (*Endpoint, error) {
	var ep Endpoint
	p := "/api/sandboxes/" + id + "/ports/" + strconv.Itoa(port)
	if err := c.do(ctx, "PUT", p, nil, struct{}{}, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

func (c *Client) UnexposePort(ctx context.Context, id string, port int) error {
	return c.do(ctx, "DELETE", "/api/sandboxes/"+id+"/ports/"+strconv.Itoa(port), nil, nil, nil)
}

type PortToken struct {
	ID        string    `json:"id"`
	Port      int       `json:"port"`
	Hostname  string    `json:"hostname"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreatePortToken grants token-authenticated HTTP/1.1 access to one port without
// exposing it publicly. A request carrying the token wakes a paused sandbox; one
// without it is refused and leaves the sandbox paused, which is what makes this
// usable as a doorbell for a service that must stay private.
func (c *Client) CreatePortToken(ctx context.Context, id string, port int, ttl string) (*PortToken, error) {
	body := map[string]any{"port": port}
	if ttl != "" {
		body["ttl"] = ttl
	}
	var t PortToken
	if err := c.do(ctx, "POST", "/api/sandboxes/"+id+"/port-tokens", nil, body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *Client) DeletePortToken(ctx context.Context, id, tokenID string) error {
	return c.do(ctx, "DELETE", "/api/sandboxes/"+id+"/port-tokens/"+tokenID, nil, nil, nil)
}

// ---- network ----

type EgressPolicy struct {
	Default      string   `json:"default"`
	Allow        []string `json:"allow,omitempty"`
	Deny         []string `json:"deny,omitempty"`
	DrainOnPause []string `json:"drain_on_pause,omitempty"`
}

type Network struct {
	Egress EgressPolicy `json:"egress"`
}

func (c *Client) GetNetwork(ctx context.Context, id string) (*Network, error) {
	var n Network
	if err := c.do(ctx, "GET", "/api/sandboxes/"+id+"/network", nil, nil, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// UpdateNetwork replaces the egress policy. This requires a Team plan; a lower
// tier returns an error whose PlanRequired reports true.
func (c *Client) UpdateNetwork(ctx context.Context, id string, n Network) error {
	return c.do(ctx, "PUT", "/api/sandboxes/"+id+"/network", nil, n, nil)
}

// AllowAllEgress is the policy every sandbox needs: the agent has to reach the
// hub, and mounting a disk requires resolving Archil's own mount control server.
func AllowAllEgress() Network {
	return Network{Egress: EgressPolicy{Default: "allow"}}
}

// AllowListEgress denies by default and permits only the given targets, with
// Archil's own endpoints always included so volume mounts keep working.
func AllowListEgress(targets []string) Network {
	allow := append([]string{}, targets...)
	allow = append(allow, RequiredEgressDomains()...)
	return Network{Egress: EgressPolicy{Default: "deny", Allow: allow}}
}

// RequiredEgressDomains are the hosts the in-guest Archil client needs. Omitting
// them from an allowlist breaks volume mounts in a way that looks like a storage
// fault, so they are added for the operator rather than left to be discovered.
func RequiredEgressDomains() []string {
	return []string{
		"mount.green.us-east-1.aws.prod.archil.com",
		"mount.green.eu-west-1.aws.prod.archil.com",
		"mount.green.us-west-2.aws.prod.archil.com",
		"mount.blue.us-central1.gcp.prod.archil.com",
	}
}
