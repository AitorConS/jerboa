package builder

import (
	"fmt"
	"runtime"
	"strings"
)

// Platform represents a target OS/architecture for building.
type Platform struct {
	OS   string
	Arch string
}

// DefaultPlatform returns the current runtime platform.
func DefaultPlatform() Platform {
	return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// String returns the platform in os/arch format (e.g. "linux/amd64").
func (p Platform) String() string {
	return p.OS + "/" + p.Arch
}

// ParsePlatform parses a string in "os/arch" format into a Platform.
func ParsePlatform(s string) (Platform, error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return Platform{}, fmt.Errorf("invalid platform %q: expected format os/arch (e.g. linux/amd64)", s)
	}
	os, arch := parts[0], parts[1]

	validOS := map[string]bool{"linux": true, "darwin": true}
	if !validOS[os] {
		return Platform{}, fmt.Errorf("unsupported os %q: use linux or darwin", os)
	}

	validArch := map[string]bool{"amd64": true, "arm64": true}
	if !validArch[arch] {
		return Platform{}, fmt.Errorf("unsupported arch %q: use amd64 or arm64", arch)
	}

	return Platform{OS: os, Arch: arch}, nil
}

// KnownPlatforms returns all supported platforms.
func KnownPlatforms() []Platform {
	return []Platform{
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
	}
}

// GoEnv returns the GOOS and GOARCH values for the platform.
func (p Platform) GoEnv() (goos, goarch string) {
	return p.OS, p.Arch
}

// GoCrossCompileEnv returns environment variables for cross-compiling Go
// binaries to this platform. CGO_ENABLED=0 is always set for static builds.
func (p Platform) GoCrossCompileEnv() []string {
	return []string{
		"CGO_ENABLED=0",
		"GOOS=" + p.OS,
		"GOARCH=" + p.Arch,
	}
}

// RustTarget returns the Rust target triple for the platform.
func (p Platform) RustTarget() string {
	switch {
	case p.OS == "linux" && p.Arch == "amd64":
		return "x86_64-unknown-linux-musl"
	case p.OS == "linux" && p.Arch == "arm64":
		return "aarch64-unknown-linux-musl"
	case p.OS == "darwin" && p.Arch == "amd64":
		return "x86_64-apple-darwin"
	case p.OS == "darwin" && p.Arch == "arm64":
		return "aarch64-apple-darwin"
	default:
		return p.Arch + "-" + p.OS
	}
}

// IsNative returns true if the platform matches the current runtime.
func (p Platform) IsNative() bool {
	cur := DefaultPlatform()
	return p.OS == cur.OS && p.Arch == cur.Arch
}
