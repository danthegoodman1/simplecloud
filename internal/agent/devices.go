package agent

import (
	"os"
	"path/filepath"
)

// prepareDevices creates the /dev file-descriptor symlinks that a sandbox does not
// ship with.
//
// An Archil sandbox has a minimal /dev with no fd entry, and many images' startup
// scripts depend on it: the official postgres entrypoint uses shell process
// substitution, which opens /dev/fd/63 and fails with a message about a missing
// file rather than a missing device. Normalizing this is part of making an
// arbitrary image run.
func (a *Agent) prepareDevices() {
	links := map[string]string{
		"/dev/fd":     "/proc/self/fd",
		"/dev/stdin":  "/proc/self/fd/0",
		"/dev/stdout": "/proc/self/fd/1",
		"/dev/stderr": "/proc/self/fd/2",
	}
	for link, target := range links {
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			continue
		}
		if err := os.Symlink(target, link); err != nil {
			a.logf("could not create %s -> %s: %v", link, target, err)
			continue
		}
		a.logf("created %s -> %s", link, target)
	}
}
