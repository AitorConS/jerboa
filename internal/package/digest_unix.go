//go:build unix

package pkg

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func statID(path string) (fileID, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return fileID{}, fmt.Errorf("stat %s: %w", path, err)
	}
	return fileID{Size: st.Size, ModNs: st.Mtim.Nano(), ChangeNs: st.Ctim.Nano(), Ino: st.Ino}, nil
}
