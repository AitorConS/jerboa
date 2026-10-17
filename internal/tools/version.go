package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const versionFileName = "kernel-version.txt"

// UnknownVersion is what LocalVersion reports when no version marker is present —
// e.g. a toolchain baked into the distro image, which ships no marker. It is not
// a real version and must not be compared against the manifest as if it were.
const UnknownVersion = "(unknown)"

// artifactNames are the remote file names that make up the kernel toolset. They
// remain here only so ClearCachedTools scrubs any legacy on-disk copies.
var artifactNames = []string{"mkfs-linux-amd64", "kernel.img", "boot.img", "dump-linux-amd64"}

const fcKernelLocalName = "kernel-fc.img"

// LocalVersion returns the semver string (e.g. "v0.1.0") cached in toolsDir.
// Returns "(unknown)" if the file is absent or unreadable.
func LocalVersion(toolsDir string) string {
	data, err := os.ReadFile(filepath.Join(toolsDir, versionFileName))
	if err != nil {
		return UnknownVersion
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return UnknownVersion
	}
	return v
}

// HasLocalVersion reports whether toolsDir carries a version marker, letting
// callers tell a known, CLI-managed toolchain from one whose version cannot be
// determined (a distro-baked toolchain) — which must not be reported as out of
// date just because its unknown version sorts below the manifest (F-006).
func HasLocalVersion(toolsDir string) bool {
	data, err := os.ReadFile(filepath.Join(toolsDir, versionFileName))
	return err == nil && strings.TrimSpace(string(data)) != ""
}

// IsNewer reports whether remote is a strictly higher semver than local.
// Unknown/malformed versions are never treated as newer.
func IsNewer(local, remote string) bool {
	return semverGT(remote, local)
}

// SaveLocalVersion writes version to toolsDir/kernel-version.txt.
func SaveLocalVersion(toolsDir, version string) error {
	path := filepath.Join(toolsDir, versionFileName)
	if err := os.WriteFile(path, []byte(version+"\n"), 0o644); err != nil {
		return fmt.Errorf("tools: save kernel version: %w", err)
	}
	return nil
}

// ClearCachedTools deletes the kernel artifacts and version file from toolsDir.
// The Firecracker kernel goes too: EnsureFCKernel only downloads it when it is
// missing, so a kept copy would outlive every kernel update (and an older one
// cannot read its command line on Firecracker >= 1.12, which boots it via PVH).
func ClearCachedTools(toolsDir string) error {
	names := append([]string{versionFileName}, artifactNames...)
	names = append(names, "mkfs", "dump", fcKernelLocalName) // local names differ from remote artifact names
	for _, name := range names {
		if err := os.Remove(filepath.Join(toolsDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("tools: clear %s: %w", name, err)
		}
	}
	return nil
}

// Exist returns true when all three kernel artifacts are present in toolsDir.
func Exist(toolsDir string) bool {
	for _, name := range []string{"mkfs", "kernel.img", "boot.img"} {
		if _, err := os.Stat(filepath.Join(toolsDir, name)); os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// FCKernelPath returns the path where the Firecracker-compatible kernel is cached.
func FCKernelPath(toolsDir string) string {
	return filepath.Join(toolsDir, fcKernelLocalName)
}

// FCKernelExists returns true when the Firecracker kernel is present in toolsDir.
func FCKernelExists(toolsDir string) bool {
	_, err := os.Stat(FCKernelPath(toolsDir))
	return err == nil
}

// EnsureFCKernel downloads kernel-fc.img named by the signed release manifest
// (SHA-256 verified) into toolsDir if it is not already present, returning its
// local path. R2 is the single source of truth — there is no GitHub fallback.
func EnsureFCKernel(ctx context.Context, toolsDir string) (string, error) {
	dest := FCKernelPath(toolsDir)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return "", fmt.Errorf("tools: create tools dir: %w", err)
	}
	if err := ensureFCKernelFromManifest(ctx, dest); err != nil {
		return "", fmt.Errorf("tools: fetch firecracker kernel: %w", err)
	}
	return dest, nil
}

// semverGT returns true when a is strictly greater than b.
// Both strings may have a leading "v". Malformed versions are treated as "0.0.0".
func semverGT(a, b string) bool {
	av := parseSemver(a)
	bv := parseSemver(b)
	for i := range av {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

func parseSemver(s string) [3]int {
	s = strings.TrimPrefix(s, "v")
	parts := strings.SplitN(s, ".", 3)
	var out [3]int
	for i, p := range parts {
		if i >= 3 {
			break
		}
		out[i], _ = strconv.Atoi(p)
	}
	return out
}
