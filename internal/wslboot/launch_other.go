//go:build !windows

package wslboot

import "syscall"

// detachAttr is a no-op off Windows; WSL bootstrap only runs on Windows, but
// the package still compiles cross-platform for tests.
func detachAttr() *syscall.SysProcAttr { return nil }
