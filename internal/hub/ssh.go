// Package hub prepares and drives the WireGuard hub over SSH.
//
// The hub stays dumb: WireGuard interfaces, nftables, and sshd. It holds no
// Archil credential and no doorbell token, so every decision is made by the CLI.
package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Client struct {
	target  string
	user    string
	host    string
	port    string
	client  *ssh.Client
	hostKey string
}

// HostKeyMismatch means the hub presented a different key than the pinned one.
// That is a reason to stop rather than a setting to update, because the
// alternative is trusting an unknown host with the project's network.
type HostKeyMismatch struct {
	Pinned    string
	Presented string
}

func (e *HostKeyMismatch) Error() string {
	return fmt.Sprintf("the hub's SSH host key changed.\n  pinned:    %s\n  presented: %s\n"+
		"  If you rebuilt the hub deliberately, run: simplecloud hub add <target> --accept-new-host-key",
		e.Pinned, e.Presented)
}

type DialOptions struct {
	User     string
	Host     string
	Port     string
	Identity string
	// PinnedHostKey is the authorized_keys-formatted key recorded on first use.
	// Empty accepts and returns whatever the host presents.
	PinnedHostKey string
	Timeout       time.Duration
}

// Dial connects and verifies the host key against the pinned one.
func Dial(ctx context.Context, o DialOptions) (*Client, error) {
	if o.Timeout == 0 {
		o.Timeout = 20 * time.Second
	}
	auths, err := authMethods(o.Identity)
	if err != nil {
		return nil, err
	}
	c := &Client{user: o.User, host: o.Host, port: o.Port, target: o.User + "@" + o.Host}

	cfg := &ssh.ClientConfig{
		User:    o.User,
		Auth:    auths,
		Timeout: o.Timeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			presented := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			c.hostKey = presented
			if o.PinnedHostKey == "" {
				return nil
			}
			if presented != strings.TrimSpace(o.PinnedHostKey) {
				return &HostKeyMismatch{Pinned: o.PinnedHostKey, Presented: presented}
			}
			return nil
		},
	}
	addr := net.JoinHostPort(o.Host, o.Port)
	d := net.Dialer{Timeout: o.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the hub at %s: %w", addr, err)
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		var mismatch *HostKeyMismatch
		if errors.As(err, &mismatch) {
			return nil, mismatch
		}
		return nil, fmt.Errorf("SSH to %s failed: %w", c.target, err)
	}
	c.client = ssh.NewClient(sc, chans, reqs)
	return c, nil
}

// HostKey is what the hub presented, for pinning on first use.
func (c *Client) HostKey() string { return c.hostKey }
func (c *Client) Target() string  { return c.target }

func (c *Client) Close() error { return c.client.Close() }

// Run executes a command and returns stdout, folding stderr into any error.
func (c *Client) Run(ctx context.Context, command string) (string, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()
	select {
	case err := <-done:
		if err != nil {
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = strings.TrimSpace(stdout.String())
			}
			return stdout.String(), fmt.Errorf("hub command failed: %s: %s", command, detail)
		}
		return stdout.String(), nil
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	}
}

// Sudo prefixes with sudo unless already root, so a passwordless-sudo user works
// as well as root.
func (c *Client) Sudo(ctx context.Context, command string) (string, error) {
	if c.user == "root" {
		return c.Run(ctx, command)
	}
	return c.Run(ctx, "sudo -n sh -c "+shellQuote(command))
}

// WriteFile writes content atomically, so a partial transfer never leaves a
// half-written configuration that the next apply would act on.
func (c *Client) WriteFile(ctx context.Context, path string, mode string, content string) error {
	tmp := path + ".sc-tmp"
	script := fmt.Sprintf(`set -eu
mkdir -p %s
cat > %s <<'SC_EOF_MARKER'
%s
SC_EOF_MARKER
chmod %s %s
mv -f %s %s`,
		shellQuote(filepath.Dir(path)), shellQuote(tmp), content, mode, shellQuote(tmp), shellQuote(tmp), shellQuote(path))
	_, err := c.Sudo(ctx, script)
	return err
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func authMethods(identity string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	var tried []string

	// Explicit identities first. An agent that errors would otherwise abort the
	// whole handshake before a perfectly good key file is ever offered.
	candidates := []string{identity}
	if identity == "" {
		candidates = nil
		if home, err := os.UserHomeDir(); err == nil {
			for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
				candidates = append(candidates, filepath.Join(home, ".ssh", n))
			}
		}
	}
	for _, path := range candidates {
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			if identity != "" {
				return nil, fmt.Errorf("reading SSH identity %s: %w", path, err)
			}
			continue
		}
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			if identity != "" {
				return nil, fmt.Errorf("parsing SSH identity %s: %w\n  An encrypted key needs ssh-agent", path, err)
			}
			continue
		}
		methods = append(methods, ssh.PublicKeys(signer))
		tried = append(tried, path)
	}

	// Then the agent, with its errors swallowed: an unreachable or empty agent must
	// not fail authentication that a key file can satisfy.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		methods = append(methods, ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
			conn, err := net.Dial("unix", sock)
			if err != nil {
				return nil, nil
			}
			defer conn.Close()
			signers, err := agent.NewClient(conn).Signers()
			if err != nil {
				return nil, nil
			}
			return signers, nil
		}))
		tried = append(tried, "ssh-agent")
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no SSH credentials.\n  Start ssh-agent, or pass --identity <path to a private key>")
	}
	return methods, nil
}
