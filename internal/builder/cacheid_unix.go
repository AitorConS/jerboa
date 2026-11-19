//go:build unix

package builder

import (
	"fmt"
	"os"
	"syscall"
)

// dirIdentity names the directory at path by its device and inode, so a
// directory removed and created again at the same path is told apart.
func dirIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
