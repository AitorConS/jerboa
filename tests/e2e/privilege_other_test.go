//go:build e2e && !linux

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireGuestNetworking is a no-op off Linux: macOS guest networks are
// userspace switches inside the daemon and need no privilege.
func requireGuestNetworking(*testing.T) {}

// platformPreflight checks what the suite needs of a macOS host.
func platformPreflight() []lack {
	var out []lack
	if _, err := exec.LookPath("qemu-system-aarch64"); err != nil {
		out = append(out, lack{"qemu-system-aarch64 is not in PATH", "brew install qemu"})
	}
	tools := suite.tools
	if tools == "" {
		tools = filepath.Join(suite.bin, "tools")
	}
	if _, err := os.Stat(filepath.Join(tools, "mkfs")); err != nil {
		out = append(out, lack{"the kernel toolchain is not in " + tools, "sh scripts/build-macos.sh"})
	}
	return out
}
