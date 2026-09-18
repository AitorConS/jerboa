//go:build !linux && !darwin

package main

import "os"

func allocatedBytes(os.FileInfo) int64 { return 0 }
