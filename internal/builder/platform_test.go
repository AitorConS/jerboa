package builder

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePlatform(t *testing.T) {
	tests := []struct {
		input  string
		os     string
		arch   string
		errMsg string
	}{
		{"linux/amd64", "linux", "amd64", ""},
		{"linux/arm64", "linux", "arm64", ""},
		{"darwin/amd64", "darwin", "amd64", ""},
		{"darwin/arm64", "darwin", "arm64", ""},
		{"windows/amd64", "", "", "unsupported os"},
		{"linux/riscv64", "", "", "unsupported arch"},
		{"amd64", "", "", "invalid platform"},
		{"", "", "", "invalid platform"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			p, err := ParsePlatform(tt.input)
			if tt.errMsg != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errMsg)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.os, p.OS)
				require.Equal(t, tt.arch, p.Arch)
			}
		})
	}
}

func TestPlatformString(t *testing.T) {
	p := Platform{OS: "linux", Arch: "amd64"}
	require.Equal(t, "linux/amd64", p.String())
}

func TestDefaultPlatform(t *testing.T) {
	p := DefaultPlatform()
	require.Equal(t, runtime.GOOS, p.OS)
	require.Equal(t, runtime.GOARCH, p.Arch)
}

func TestPlatformGoEnv(t *testing.T) {
	p := Platform{OS: "linux", Arch: "arm64"}
	goos, goarch := p.GoEnv()
	require.Equal(t, "linux", goos)
	require.Equal(t, "arm64", goarch)
}

func TestPlatformGoCrossCompileEnv(t *testing.T) {
	p := Platform{OS: "linux", Arch: "arm64"}
	env := p.GoCrossCompileEnv()
	require.Contains(t, env, "CGO_ENABLED=0")
	require.Contains(t, env, "GOOS=linux")
	require.Contains(t, env, "GOARCH=arm64")
}

func TestPlatformRustTarget(t *testing.T) {
	tests := []struct {
		platform Platform
		target   string
	}{
		{Platform{OS: "linux", Arch: "amd64"}, "x86_64-unknown-linux-musl"},
		{Platform{OS: "linux", Arch: "arm64"}, "aarch64-unknown-linux-musl"},
		{Platform{OS: "darwin", Arch: "amd64"}, "x86_64-apple-darwin"},
		{Platform{OS: "darwin", Arch: "arm64"}, "aarch64-apple-darwin"},
	}

	for _, tt := range tests {
		t.Run(tt.platform.String(), func(t *testing.T) {
			require.Equal(t, tt.target, tt.platform.RustTarget())
		})
	}
}

func TestPlatformIsNative(t *testing.T) {
	native := DefaultPlatform()
	require.True(t, native.IsNative())

	foreign := Platform{OS: "linux", Arch: "arm64"}
	if native.OS == "linux" && native.Arch == "arm64" {
		require.True(t, foreign.IsNative())
	} else {
		require.False(t, foreign.IsNative())
	}
}

func TestKnownPlatforms(t *testing.T) {
	platforms := KnownPlatforms()
	require.Len(t, platforms, 2)
	require.Contains(t, platforms, Platform{OS: "linux", Arch: "amd64"})
	require.Contains(t, platforms, Platform{OS: "linux", Arch: "arm64"})
}
