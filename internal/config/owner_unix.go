//go:build unix

package config

import (
	"crypto/rand"
	"encoding/hex"
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
	dirfd, leaf, err := openPrivateDir(root, path, false)
	if err != nil {
		return nil, err
	}
	// O_NONBLOCK keeps a FIFO planted at the path from blocking the open.
	fd, err := unix.Openat(dirfd, leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	_ = unix.Close(dirfd)
	if err != nil && !errors.Is(err, unix.ELOOP) {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	if err != nil || !private(fd, os.Getuid(), unix.S_IFREG) {
		if err == nil {
			_ = unix.Close(fd)
		}
		return nil, fmt.Errorf("config: %s is not a private file owned by this user; refusing to read it", path)
	}
	f := os.NewFile(uintptr(fd), path) //nolint:gosec // fd is a valid descriptor from openat
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// writePrivate replaces path, which lies below root, with data (mode 0600).
// The directories below root are created 0700 where missing and must be real
// directories owned by this user; one that others can write is narrowed to
// 0700 through its descriptor. The data goes to a fresh O_EXCL temporary file
// in the checked directory, which is then renamed over the target there, so
// neither a symlink nor a file planted at the target is ever written through.
func writePrivate(root, path string, data []byte) (err error) {
	dirfd, leaf, err := openPrivateDir(root, path, true)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dirfd) }()

	var suffix [8]byte
	_, _ = rand.Read(suffix[:]) // crypto/rand.Read does not fail
	tmp := leaf + ".tmp-" + hex.EncodeToString(suffix[:])
	fd, err := unix.Openat(dirfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(filepath.Dir(path), tmp)) //nolint:gosec // fd is a valid descriptor from openat
	defer func() {
		if err != nil {
			_ = unix.Unlinkat(dirfd, tmp, 0)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	if err = unix.Renameat(dirfd, tmp, dirfd, leaf); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	if err := unix.Fsync(dirfd); err != nil {
		return fmt.Errorf("config: sync %s: %w", filepath.Dir(path), err)
	}
	return nil
}

// openPrivateDir opens the directory holding path, walking from root one
// component at a time with O_NOFOLLOW and checking each directory with fstat.
// It returns that directory's descriptor and the file's name in it. With
// create, missing directories are made 0700 and an owned directory others can
// write is narrowed to 0700; without it, such a directory is refused and a
// missing one is reported as not existing.
func openPrivateDir(root, path string, create bool) (int, string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return -1, "", fmt.Errorf("config: %s is not below %s", path, root)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	uid := os.Getuid()

	dirfd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", &fs.PathError{Op: "open", Path: root, Err: err}
	}
	cur := root
	for _, name := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, name)
		if create {
			if err := unix.Mkdirat(dirfd, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(dirfd)
				return -1, "", &fs.PathError{Op: "mkdir", Path: cur, Err: err}
			}
		}
		fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(dirfd)
		if errors.Is(err, unix.ENOENT) {
			return -1, "", &fs.PathError{Op: "open", Path: cur, Err: err}
		}
		// Any other failure (ELOOP, ENOTDIR) means a symlink or a non-directory.
		ok := err == nil && private(fd, uid, unix.S_IFDIR)
		if err == nil && !ok && create && owned(fd, uid, unix.S_IFDIR) {
			// This user's own directory, made writable by others: narrow it
			// on the descriptor, which cannot have been swapped.
			ok = unix.Fchmod(fd, 0o700) == nil
		}
		if !ok {
			if err == nil {
				_ = unix.Close(fd)
			}
			return -1, "", fmt.Errorf("config: %s is not a private directory owned by this user; refusing to use it", cur)
		}
		dirfd = fd
	}
	return dirfd, parts[len(parts)-1], nil
}

// owned reports whether the open descriptor fd is of type kind (S_IFDIR or
// S_IFREG) and owned by uid.
func owned(fd, uid int, kind uint32) bool {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false
	}
	return uint32(st.Mode)&unix.S_IFMT == kind && int(st.Uid) == uid
}

// private reports whether the open descriptor fd is of type kind, owned by
// uid and not writable by group or others.
func private(fd, uid int, kind uint32) bool {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false
	}
	return uint32(st.Mode)&unix.S_IFMT == kind && int(st.Uid) == uid && st.Mode&0o022 == 0
}
