//go:build linux

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// dropPageCache advises the kernel that written pages are not needed, which keeps
// the log off the pause snapshot.
func dropPageCache(f *os.File) {
	unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}
