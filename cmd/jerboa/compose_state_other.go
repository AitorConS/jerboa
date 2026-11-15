//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// checkStateFileOwner refuses a compose state file that another user wrote
// or could have changed: it must be owned by the user running the CLI and
// writable by nobody else.
func checkStateFileOwner(fi os.FileInfo) error {
	if fi.Mode().Perm()&0o022 != 0 {
		return errors.New("it is writable by other users")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return errors.New("it is owned by another user")
	}
	return nil
}
