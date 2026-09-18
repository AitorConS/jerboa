//go:build linux

package diskclone

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile attempts a FICLONE reflink (btrfs, XFS with reflink=1, bcachefs).
// Any failure (unsupported filesystem, cross-device paths) reports false so
// the caller falls back to a sparse copy, which surfaces genuine I/O errors.
func cloneFile(dst, src string) bool {
	in, err := os.Open(src) //nolint:gosec // daemon-owned image store path
	if err != nil {
		return false
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // caller-owned path
	if err != nil {
		return false
	}
	cloneErr := unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	closeErr := out.Close()
	if cloneErr != nil || closeErr != nil {
		_ = os.Remove(dst)
		return false
	}
	return true
}
