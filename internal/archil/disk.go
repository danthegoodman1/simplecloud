package archil

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

type Disk struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Region string `json:"region"`
}

type CreateDiskResult struct {
	Disk  Disk
	Token string
}

// createDiskResponse is the wire shape, which differs from the list and get
// shapes: the id is diskId and the mount token arrives inside authorizedUsers.
type createDiskResponse struct {
	DiskID          string `json:"diskId"`
	AuthorizedUsers []struct {
		Type     string `json:"type"`
		Token    string `json:"token"`
		ReadOnly bool   `json:"readOnly"`
	} `json:"authorizedUsers"`
}

// CreateDisk makes a disk and returns its one-time mount token. The token is shown
// exactly once, so the caller must persist it.
func (c *Client) CreateDisk(ctx context.Context, name string) (*CreateDiskResult, error) {
	var raw createDiskResponse
	if err := c.do(ctx, "POST", "/api/disks", nil, map[string]any{"name": name}, &raw); err != nil {
		return nil, err
	}
	out := &CreateDiskResult{Disk: Disk{ID: raw.DiskID, Name: name}}
	for _, u := range raw.AuthorizedUsers {
		if u.Type == "token" && u.Token != "" && !u.ReadOnly {
			out.Token = u.Token
			break
		}
	}
	if out.Disk.ID == "" {
		return nil, fmt.Errorf("archil: creating disk %q returned no id", name)
	}
	if out.Token == "" {
		return nil, fmt.Errorf("archil: disk %s was created without a writable mount token", out.Disk.ID)
	}
	return out, nil
}

func (c *Client) ListDisks(ctx context.Context) ([]Disk, error) {
	var disks []Disk
	q := url.Values{"limit": {"100"}}
	if err := c.do(ctx, "GET", "/api/disks", q, nil, &disks); err != nil {
		return nil, err
	}
	return disks, nil
}

func (c *Client) GetDisk(ctx context.Context, id string) (*Disk, error) {
	var d Disk
	if err := c.do(ctx, "GET", "/api/disks/"+id, nil, nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (c *Client) DeleteDisk(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/disks/"+id, nil, nil, nil)
}

// CreateDiskToken mints an additional mount token, for handing a disk to a new
// owner during a volume handover.
func (c *Client) CreateDiskToken(ctx context.Context, diskID, nickname string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	body := map[string]any{"type": "token", "nickname": nickname}
	if err := c.do(ctx, "POST", "/api/disks/"+diskID+"/users", nil, body, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("archil: disk %s returned no token", diskID)
	}
	return out.Token, nil
}

type Delegation struct {
	ClientID   string `json:"clientId"`
	InodeID    int64  `json:"inodeId"`
	Path       string `json:"path"`
	IsPending  bool   `json:"isPending"`
	IsOrphaned bool   `json:"isOrphaned"`
}

func (c *Client) ListDelegations(ctx context.Context, diskID string) ([]Delegation, error) {
	var out struct {
		Delegations []Delegation `json:"delegations"`
	}
	if err := c.do(ctx, "GET", "/api/disks/"+diskID+"/delegations", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Delegations, nil
}

// RevokeDelegation forcibly takes a write delegation away from its holder.
//
// The revoked client is not fenced immediately: it keeps accepting writes that
// report success and are never durable, until another client acquires the
// delegation. It also serves a stale read cache. So a handover must stop the old
// owner before revoking, and must never read from a revoked owner. See
// WaitForNoDelegations.
func (c *Client) RevokeDelegation(ctx context.Context, diskID string, d Delegation) error {
	body := map[string]any{"clientId": d.ClientID, "inodeId": d.InodeID}
	return c.do(ctx, "POST", "/api/disks/"+diskID+"/revoke-delegation", nil, body, nil)
}

// WaitForNoDelegations revokes every delegation on a disk and confirms the list
// is empty before returning, which is the only safe precondition for a new owner.
func (c *Client) WaitForNoDelegations(ctx context.Context, diskID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ds, err := c.ListDelegations(ctx, diskID)
		if err != nil {
			return err
		}
		if len(ds) == 0 {
			return nil
		}
		for _, d := range ds {
			if err := c.RevokeDelegation(ctx, diskID, d); err != nil {
				return err
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("archil: disk %s still has %d delegation(s) after %s", diskID, len(ds), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
