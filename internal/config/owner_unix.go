//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// checkOwner refuses a file another user owns or can write: in a shared
// directory such a file could point the client at an endpoint of that user's
// choosing, and so receive this user's token.
func checkOwner(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("config: %s is not a private file owned by this user; refusing to read it", path)
	}
	return nil
}
