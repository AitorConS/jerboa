//go:build unix

package builder

import (
	"fmt"
	"os"
	"syscall"
)

// OpenRegular opens path for reading without following a symlink in its
// last component, and returns the file with its information. It fails unless
// path is a regular file. A project's build steps can replace one of its
// files with a link to a host file at any moment; opening the file this way,
// and comparing what was opened with what was checked earlier, keeps the link
// from being followed. O_NONBLOCK keeps a FIFO put in a file's place from
// blocking the open; it does not change how a regular file is read.
func OpenRegular(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // callers check what was opened
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("open %s: not a regular file", path)
	}
	return f, info, nil
}
