//go:build e2e && !linux

package e2e

import "testing"

// requireGuestNetworking is a no-op off Linux: macOS guest networks are
// userspace switches inside the daemon and need no privilege.
func requireGuestNetworking(*testing.T) {}
