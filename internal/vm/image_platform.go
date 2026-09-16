//go:build linux || (darwin && arm64)

package vm

import (
	"fmt"
	"runtime"
)

// ValidateImagePlatform runs before resource reservation or volume preparation.
func (m *FirecrackerManager) ValidateImagePlatform(architecture string, emulate bool) error {
	if architecture == "" || architecture == "x86_64" {
		architecture = "amd64"
	}
	if emulate {
		return errFCX86Emulation()
	}
	if architecture != runtime.GOARCH {
		return fmt.Errorf("firecracker on %s requires a %s image, got %s", runtime.GOOS, runtime.GOARCH, architecture)
	}
	return nil
}

// errFCX86Emulation explains how to reach QEMU, the only backend that emulates x86.
func errFCX86Emulation() error {
	if runtime.GOOS == "darwin" {
		return fmt.Errorf("firecracker/HVF cannot emulate x86 images; rebuild with --platform linux/arm64, " +
			"or switch the daemon to QEMU (brew install qemu, then jerboa config set hypervisor qemu && jerboa daemon restart, " +
			"or select QEMU in Jerboa Desktop settings)")
	}
	return fmt.Errorf("firecracker does not support x86 emulation; select QEMU explicitly")
}
func (m *QEMUManager) ValidateImagePlatform(architecture string, emulate bool) error {
	if architecture == "" || architecture == "x86_64" {
		architecture = "amd64"
	}
	if architecture != "arm64" && architecture != "amd64" {
		return fmt.Errorf("unsupported image architecture %q", architecture)
	}
	if runtime.GOOS == "darwin" {
		if architecture == "amd64" && !emulate {
			return fmt.Errorf("x86_64 image requires explicit --emulate-x86 on macOS")
		}
		if architecture == "arm64" && emulate {
			return fmt.Errorf("--emulate-x86 conflicts with ARM64 image")
		}
	}
	return nil
}
