// Package archil is a client for the Archil control plane and sandbox process API.
//
// There is no official Go SDK. The REST shapes and the process WebSocket framing
// here were derived from the published Python SDK and verified against the live API.
package archil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Regions maps the region names the CLI accepts to control plane base URLs.
var Regions = map[string]string{
	"aws-us-east-1":   "https://control.green.us-east-1.aws.prod.archil.com",
	"aws-eu-west-1":   "https://control.green.eu-west-1.aws.prod.archil.com",
	"aws-us-west-2":   "https://control.green.us-west-2.aws.prod.archil.com",
	"gcp-us-central1": "https://control.blue.us-central1.gcp.prod.archil.com",
}

// DefaultRegion is used when none is configured.
const DefaultRegion = "aws-us-east-1"

type Client struct {
	apiKey  string
	region  string
	baseURL string
	http    *http.Client
}

func New(apiKey, region string) (*Client, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("archil: no API key; set ARCHIL_API_KEY")
	}
	if region == "" {
		region = DefaultRegion
	}
	base, ok := Regions[region]
	if !ok {
		names := make([]string, 0, len(Regions))
		for n := range Regions {
			names = append(names, n)
		}
		return nil, fmt.Errorf("archil: unknown region %q; known regions are %s", region, strings.Join(names, ", "))
	}
	return &Client{
		apiKey:  apiKey,
		region:  region,
		baseURL: base,
		http:    &http.Client{Timeout: 120 * time.Second},
	}, nil
}

func (c *Client) Region() string { return c.region }

// APIError is a structured error from the control plane.
type APIError struct {
	Status  int
	Code    string
	Message string
	Path    string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("archil %s: %s (%s, HTTP %d)", e.Path, e.Message, e.Code, e.Status)
	}
	return fmt.Sprintf("archil %s: %s (HTTP %d)", e.Path, e.Message, e.Status)
}

// PlanRequired reports whether the failure was the account's plan tier rather
// than the request. Egress needs a Team plan, and the message is easy to miss.
func (e *APIError) PlanRequired() bool {
	return strings.Contains(strings.ToLower(e.Message), "paid plan")
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
	Code    string          `json:"code"`
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("archil %s: encoding request: %w", path, err)
		}
		reader = bytes.NewReader(raw)
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return fmt.Errorf("archil %s: %w", path, err)
	}
	req.Header.Set("Authorization", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("archil %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("archil %s: reading response: %w", path, err)
	}
	var env envelope
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &env)
	}
	if resp.StatusCode >= 300 || (len(raw) > 0 && !env.Success) {
		msg := env.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if msg == "" {
			msg = resp.Status
		}
		return &APIError{Status: resp.StatusCode, Code: env.Code, Message: msg, Path: path}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("archil %s: decoding response: %w", path, err)
		}
	}
	return nil
}
