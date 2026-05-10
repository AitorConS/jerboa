//go:build !linux

package network

import "errors"

// TAPConfig holds the parameters for a TAP device.
type TAPConfig struct {
	Name   string
	Bridge string
}

// TAP is unavailable on non-Linux platforms.
type TAP struct{}

// Create is unavailable on non-Linux platforms.
func Create(_ TAPConfig) (*TAP, error) {
	return nil, errors.New("TAP creation requires Linux")
}

// Name returns an empty interface name on non-Linux platforms.
func (t *TAP) Name() string {
	_ = t
	return ""
}

// Destroy is unavailable on non-Linux platforms.
func (t *TAP) Destroy() error {
	_ = t
	return errors.New("TAP destruction requires Linux")
}
