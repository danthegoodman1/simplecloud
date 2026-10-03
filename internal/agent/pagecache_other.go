//go:build !linux

package agent

import "os"

// dropPageCache is a no-op away from Linux; the agent only ever runs in a sandbox.
func dropPageCache(f *os.File) {}
