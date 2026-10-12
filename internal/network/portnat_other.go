//go:build !linux

package network

import "errors"

// Kernel NAT for published ports exists only for Linux bridges; macOS
// networks publish through their userspace gateway.
func newKernelNAT() (portNAT, error) { return nil, errors.New("not Linux") }
