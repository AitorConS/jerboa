//go:build linux || (darwin && arm64)

package vm

import (
	"fmt"
	"os"
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

// imageLayoutCompact mirrors image.LayoutCompact (vm does not import image).
const imageLayoutCompact = "compact"

// ValidateImageLayout rejects image layouts QEMU cannot boot. A standard image
// boots through its own boot code. A compact image has none, so QEMU loads the
// kernel itself: the ARM64 kernel natively on macOS, and on Linux the x86 PVH
// kernel from the tools directory. The x86 PVH entry only boots with KVM, not
// under TCG emulation, so x86 emulation on macOS and Linux hosts without KVM
// need standard images.
func (m *QEMUManager) ValidateImageLayout(layout string, emulate bool) error {
	if layout != imageLayoutCompact {
		return nil
	}
	if runtime.GOOS == "darwin" {
		if emulate {
			return fmt.Errorf("x86 emulation boots through the BIOS and cannot boot compact images; rebuild with --layout standard")
		}
		return nil
	}
	if len(kvmAccelArgs()) == 0 {
		return fmt.Errorf("compact images boot through KVM on QEMU, which is unavailable; rebuild with --layout standard or enable KVM")
	}
	if m.kernelPath == "" {
		return fmt.Errorf("compact image needs a kernel to load directly; start the daemon with a tools directory")
	}
	if info, err := os.Stat(m.kernelPath); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("compact image needs the x86 kernel at %s; run jerboa kernel update or rebuild with --layout standard", m.kernelPath)
	}
	return nil
}

// ValidateImageLayout accepts every layout: Firecracker always loads the
// kernel directly and only reads the root filesystem from the image.
func (m *FirecrackerManager) ValidateImageLayout(_ string, _ bool) error {
	return nil
}
