//go:build unix

package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// readPrivate reads path, which lies below root, only if it is a regular file
// this user owns and nobody else can write, reached through directories that
// are likewise this user's and not writable by anyone else. In a shared
// directory such a file could otherwise point the client at an endpoint of
// another user's choosing, and so receive this user's token.
//
// Every component below root is opened with O_NOFOLLOW relative to the
// directory before it, and each check is made with fstat on the descriptor
// that is then used, so nothing can be swapped in between check and read.
// root itself is trusted and may be a symlink (macOS's /var/folders is).
func readPrivate(root, path string) ([]byte, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("config: %s is not below %s", path, root)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	uid := os.Getuid()

	dirfd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: root, Err: err}
	}
	cur := root
	for _, name := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, name)
		fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(dirfd)
		if errors.Is(err, unix.ENOENT) {
			return nil, &fs.PathError{Op: "open", Path: cur, Err: err}
		}
		// Any other failure (ELOOP, ENOTDIR) means a symlink or a non-directory.
		if err != nil || !private(fd, uid, unix.S_IFDIR) {
			if err == nil {
				_ = unix.Close(fd)
			}
			return nil, fmt.Errorf("config: %s is not a private directory owned by this user; refusing to use it", cur)
		}
		dirfd = fd
	}

	// O_NONBLOCK keeps a FIFO planted at the path from blocking the open.
	fd, err := unix.Openat(dirfd, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	_ = unix.Close(dirfd)
	if err != nil && !errors.Is(err, unix.ELOOP) {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	if err != nil || !private(fd, uid, unix.S_IFREG) {
		if err == nil {
			_ = unix.Close(fd)
		}
		return nil, fmt.Errorf("config: %s is not a private file owned by this user; refusing to read it", path)
	}
	f := os.NewFile(uintptr(fd), path) //nolint:gosec // fd is a valid descriptor from openat
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// private reports whether the open descriptor fd is of type kind (S_IFDIR or
// S_IFREG), owned by uid and not writable by group or others.
func private(fd, uid int, kind uint32) bool {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false
	}
	return uint32(st.Mode)&unix.S_IFMT == kind && int(st.Uid) == uid && st.Mode&0o022 == 0
}
