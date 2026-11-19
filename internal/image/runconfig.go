package image

import (
	"fmt"
	"strconv"
	"strings"
)

// Bounds of the run defaults an image may carry.
const (
	// MaxCPUs is the most virtual CPUs an image may ask for by default.
	MaxCPUs = 256
	// MaxMemoryBytes is the most memory an image may ask for by default.
	MaxMemoryBytes int64 = 1 << 40
	// MaxMinDiskSize is the largest boot disk an image may ask for.
	MaxMinDiskSize int64 = 16 << 40
)

// ValidateRunConfig checks the run defaults of an image that came from
// elsewhere (a registry), which reach the hypervisor's command line: the
// memory is a number of MiB, or of k, m, g or t (binary units, an optional
// "i", any case), from 1 MiB to MaxMemoryBytes; the CPUs, zero for unset, at
// most MaxCPUs; the boot disk size at most MaxMinDiskSize. This accepts what
// unikernel.toml accepts ("256M", "1G", "512Mi") and the bare MiB counts
// `jerboa build --memory` has always passed through.
func ValidateRunConfig(c Config) error {
	if c.Memory != "" {
		if _, err := MemoryBytes(c.Memory); err != nil {
			return err
		}
	}
	if c.CPUs < 0 || c.CPUs > MaxCPUs {
		return fmt.Errorf("invalid default CPU count %d: want 1 to %d", c.CPUs, MaxCPUs)
	}
	if c.MinDiskSize < 0 || c.MinDiskSize > MaxMinDiskSize {
		return fmt.Errorf("invalid minimum disk size %d: want at most %d bytes", c.MinDiskSize, MaxMinDiskSize)
	}
	return nil
}

// MemoryBytes parses a memory size as ValidateRunConfig accepts it.
func MemoryBytes(s string) (int64, error) {
	invalid := fmt.Errorf("invalid default memory %q: use e.g. 256M or 1G, from 1M to 1T", s)
	t := strings.ToLower(s)
	binary := strings.HasSuffix(t, "i") // "Mi", "Gi": the same units
	t = strings.TrimSuffix(t, "i")
	units := map[byte]int64{'k': 1 << 10, 'm': 1 << 20, 'g': 1 << 30, 't': 1 << 40}
	mul := int64(1 << 20) // a bare number is MiB, as QEMU and Firecracker read it
	if n := len(t); n > 0 && units[t[n-1]] != 0 {
		mul, t = units[t[n-1]], t[:n-1]
	} else if binary {
		return 0, invalid
	}
	if t == "" || t[0] == '0' || len(t) > 13 || strings.TrimLeft(t, "0123456789") != "" {
		return 0, invalid
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n > MaxMemoryBytes/mul {
		return 0, invalid
	}
	if b := n * mul; b >= 1<<20 {
		return b, nil
	}
	return 0, invalid
}
