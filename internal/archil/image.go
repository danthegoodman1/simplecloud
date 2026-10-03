package archil

import (
	"context"
	"fmt"
	"time"
)

type RegistryAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type Image struct {
	ImageID         string `json:"image_id"`
	Source          string `json:"source"`
	Private         bool   `json:"private"`
	Status          string `json:"status"`
	Digest          string `json:"digest"`
	CanonicalSource string `json:"canonical_source"`
	FailureReason   string `json:"failure_reason"`
}

// ResolveImage pins an OCI reference to a digest, waiting until it is ready.
//
// Archil neither builds images nor hosts a registry: this fetches a reference from
// wherever it already lives and returns an id a sandbox can be created from.
// Resolution is asynchronous, and creating a sandbox from an image that is still
// building is rejected, so this polls rather than handing back a half-ready id.
// Auth is required on every call for a private image.
func (c *Client) ResolveImage(ctx context.Context, source string, auth *RegistryAuth) (*Image, error) {
	// The field is source; the API's own error for a missing one names base_image,
	// which is misleading rather than a second accepted name.
	body := map[string]any{"source": source}
	if auth != nil {
		body["registry_auth"] = auth
	}
	var img Image
	if err := c.do(ctx, "POST", "/api/images", nil, body, &img); err != nil {
		return nil, err
	}
	return c.waitForImage(ctx, &img, 20*time.Minute)
}

func (c *Client) waitForImage(ctx context.Context, img *Image, timeout time.Duration) (*Image, error) {
	deadline := time.Now().Add(timeout)
	for {
		switch img.Status {
		case "ready":
			return img, nil
		case "failed":
			reason := img.FailureReason
			if reason == "" {
				reason = "no reason given"
			}
			return nil, fmt.Errorf("archil: resolving image %s failed: %s", img.Source, reason)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("archil: image %s was still %s after %s", img.Source, img.Status, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		next, err := c.GetImage(ctx, img.ImageID)
		if err != nil {
			return nil, err
		}
		img = next
	}
}

func (c *Client) GetImage(ctx context.Context, imageID string) (*Image, error) {
	var img Image
	if err := c.do(ctx, "GET", "/api/images/"+imageID, nil, nil, &img); err != nil {
		return nil, err
	}
	return &img, nil
}
