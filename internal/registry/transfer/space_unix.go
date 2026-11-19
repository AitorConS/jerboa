//go:build linux || darwin

package transfer

import (
	"os"
	"syscall"
)

// statfsSpace returns the space available to this process on the filesystem
// holding dir, and an identifier of that filesystem. ok is false when it
// cannot tell, and the pull then goes ahead without the check.
func statfsSpace(dir string) (free int64, fs uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	info, err := os.Stat(dir)
	if err != nil {
		return 0, 0, false
	}
	sys, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	bs := int64(st.Bsize)                               //nolint:unconvert // Bsize differs in type across platforms
	return int64(st.Bavail) * bs, uint64(sys.Dev), true //nolint:gosec,unconvert // block counts fit in int64; Dev differs in type across platforms
}
