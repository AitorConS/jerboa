//go:build darwin

package diskclone

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile attempts an APFS clonefile. Any failure (HFS+, cross-volume paths)
// reports false so the caller falls back to a sparse copy.
func cloneFile(dst, src string) bool {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err != nil {
		_ = os.Remove(dst)
		return false
	}
	return true
}
