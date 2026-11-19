//go:build !unix

package builder

import (
	"fmt"
	"os"
)

// OpenRegular opens path for reading if it is a regular file, not a link,
// and returns the file with its information. Without O_NOFOLLOW the path is
// checked with Lstat first and the opened file must be the one checked.
func OpenRegular(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("open %s: not a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // checked against the Lstat above
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("open %s: the file changed while it was opened", path)
	}
	return f, info, nil
}
