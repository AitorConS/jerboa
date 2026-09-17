package image

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// Disk image layouts (Manifest.Layout).
const (
	// LayoutStandard is the BIOS-bootable layout: boot code, a boot
	// filesystem holding the kernel, and the root filesystem. It is stored as
	// an empty string so manifests written before layouts existed match it.
	LayoutStandard = ""
	// LayoutCompact has a minimal MBR and only the root filesystem. It boots
	// only where the hypervisor loads the kernel itself: Firecracker, and QEMU
	// direct kernel boot (native ARM64 on macOS). It is smaller and does not
	// ship a second copy of the kernel inside every image.
	LayoutCompact = "compact"
)

// ParseLayout maps a user-facing layout name to its manifest value.
func ParseLayout(s string) (string, error) {
	switch s {
	case "", "standard":
		return LayoutStandard, nil
	case LayoutCompact:
		return LayoutCompact, nil
	default:
		return "", fmt.Errorf("invalid image layout %q (want standard or compact)", s)
	}
}

// MBR layout, see kernel/src/runtime/storage.h.
const (
	mbrSize             = 512
	mbrPartitionTable   = 446
	mbrPartitionEntry   = 16
	mbrEntryNSectorsOff = 12
)

// checkImageLayout verifies that the image at path has the requested layout.
// An mkfs that predates the compact layout ignores the request and silently
// produces a standard image; this turns that into a clear build error.
func checkImageLayout(path, layout string) error {
	if layout != LayoutCompact {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // build temp file
	if err != nil {
		return fmt.Errorf("check image layout: %w", err)
	}
	defer func() { _ = f.Close() }()
	mbr := make([]byte, mbrSize)
	if _, err := io.ReadFull(f, mbr); err != nil {
		return fmt.Errorf("check image layout: read MBR: %w", err)
	}
	nsectors := func(i int) uint32 {
		off := mbrPartitionTable + i*mbrPartitionEntry + mbrEntryNSectorsOff
		return binary.LittleEndian.Uint32(mbr[off : off+4])
	}
	if mbr[510] != 0x55 || mbr[511] != 0xaa || nsectors(0) != 0 || nsectors(1) == 0 {
		return fmt.Errorf("mkfs did not produce the compact layout; update the kernel toolchain (jerboa kernel update) or build with --layout standard")
	}
	return nil
}
