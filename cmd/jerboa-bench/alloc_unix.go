//go:build linux || darwin

package main

import (
	"os"
	"syscall"
)

func allocatedBytes(st os.FileInfo) int64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Blocks * 512
	}
	return 0
}
