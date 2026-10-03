package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// MountVolumes attaches every declared disk before the application starts.
//
// A mount failure is fatal rather than a warning: starting a database with an
// empty data directory looks like success and fails much later, in a way nobody
// can diagnose.
func (a *Agent) MountVolumes(ctx context.Context) error {
	for _, m := range a.cfg.Mounts {
		if err := a.mountOne(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) mountOne(ctx context.Context, m Mount) error {
	if err := os.MkdirAll(m.Path, 0o755); err != nil {
		return err
	}
	if mounted(m.Path) {
		a.logf("%s already mounted", m.Path)
		return nil
	}
	region := m.Region
	if region == "" {
		region = a.cfg.Region
	}
	deadline := time.Now().Add(3 * time.Minute)
	var last error
	for attempt := 1; ; attempt++ {
		c, cancel := context.WithTimeout(ctx, 90*time.Second)
		cmd := exec.CommandContext(c, "archil", "mount", m.DiskID, m.Path, "--region", region)
		cmd.Env = append(os.Environ(), "ARCHIL_MOUNT_TOKEN="+m.Token)
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil && mounted(m.Path) {
			a.logf("mounted %s at %s", m.DiskID, m.Path)
			return nil
		}
		text := strings.TrimSpace(string(out))
		last = fmt.Errorf("mounting %s at %s: %v: %s", m.DiskID, m.Path, err, lastLine(text))
		// A name that will not resolve is the symptom of a restrictive egress
		// allowlist, so say that rather than letting it read as a storage fault.
		if strings.Contains(text, "lookup address information") || strings.Contains(text, "Invalid control server address") {
			return fmt.Errorf("%w\n  The agent cannot resolve Archil's mount server.\n"+
				"  If this service sets x-simplecloud-egress, Archil's own endpoints must be allowed", last)
		}
		if time.Now().After(deadline) {
			return last
		}
		a.logf("mount attempt %d failed, retrying: %s", attempt, lastLine(text))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// mounted reports whether path is a mount point.
//
// This is the only reliable confirmation. A failed mount leaves a writable
// directory that looks identical to a successful one, and writes to it land on the
// sandbox root instead of the disk — which reads as success until the data is
// expected somewhere else.
func mounted(path string) bool {
	if err := exec.Command("mountpoint", "-q", path).Run(); err == nil {
		return true
	}
	// Compare device numbers when mountpoint is absent: a mount point differs from
	// its own parent directory.
	var here, parent unix.Stat_t
	if err := unix.Stat(path, &here); err != nil {
		return false
	}
	if err := unix.Stat(filepath.Dir(strings.TrimSuffix(path, "/")), &parent); err != nil {
		return false
	}
	return here.Dev != parent.Dev
}

func lastLine(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
