package tools

import (
	"debug/elf"
	"debug/macho"
	"fmt"
	"os"
	"path/filepath"
)

// ValidateNativeTools prevents a cached Linux mkfs from being executed on macOS.
func ValidateNativeTools(dir string) error {
	f, err := macho.Open(filepath.Join(dir, "mkfs"))
	if err != nil {
		return fmt.Errorf("macOS requires native ARM64 mkfs: %w", err)
	}
	defer f.Close()
	if f.Cpu != macho.CpuArm64 {
		return fmt.Errorf("mkfs must be macOS ARM64, got %s", f.Cpu)
	}
	marker, err := os.ReadFile(filepath.Join(dir, "platform.txt"))
	if err != nil || string(marker) != "darwin-arm64\n" {
		return fmt.Errorf("kernel toolset has no darwin-arm64 platform marker; build with scripts/build-macos.sh")
	}
	k, err := elf.Open(filepath.Join(dir, "kernel.img"))
	if err != nil {
		return fmt.Errorf("invalid ARM64 kernel: %w", err)
	}
	defer k.Close()
	if k.Machine != elf.EM_AARCH64 {
		return fmt.Errorf("native kernel must be AArch64, got %s", k.Machine)
	}
	return nil
}
