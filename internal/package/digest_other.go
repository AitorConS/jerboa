//go:build !unix

package pkg

import (
	"fmt"
	"os"
)

// statID falls back to size and modification time, so cached digests are
// only reused within one process.
func statID(path string) (fileID, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileID{}, fmt.Errorf("stat %s: %w", path, err)
	}
	return fileID{Size: info.Size(), ModNs: info.ModTime().UnixNano()}, nil
}
