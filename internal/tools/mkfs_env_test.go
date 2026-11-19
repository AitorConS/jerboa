//go:build !windows

package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// envDumpingMkfs writes an mkfs that records its environment to the returned
// file and makes no disk.
func envDumpingMkfs(t *testing.T) (toolsDir, envFile string) {
	t.Helper()
	toolsDir = t.TempDir()
	envFile = filepath.Join(t.TempDir(), "env")
	script := "#!/bin/sh\ncat > /dev/null\nenv > '" + envFile + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "mkfs"), []byte(script), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "kernel.img"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "boot.img"), []byte("x"), 0o644))
	return toolsDir, envFile
}

func requireNoTokens(t *testing.T, envFile string) {
	t.Helper()
	data, err := os.ReadFile(envFile)
	require.NoError(t, err)
	require.NotContains(t, string(data), "s3cr3t")
	for _, line := range strings.Split(string(data), "\n") {
		require.False(t, strings.HasPrefix(line, "JERBOA_"), "mkfs got %q", line)
	}
}

// The daemon's tokens and its other variables do not reach mkfs, which reads
// packages built from someone else's files.
func TestVolumeMkfsGetsNoDaemonCredentials(t *testing.T) {
	for _, name := range []string{"JERBOA_AUTH_TOKEN", "JERBOA_CLUSTER_TOKEN", "JERBOA_OBSERVABILITY_TOKEN", "JERBOA_GUEST_HOST_ACCESS"} {
		t.Setenv(name, "s3cr3t")
	}
	if runtime.GOOS == "darwin" {
		t.Skip("macOS accepts only a native ARM64 toolset, not a script")
	}
	ctx := context.Background()

	toolsDir, envFile := envDumpingMkfs(t)
	format, err := ResolveVolumeFormatter(ctx, toolsDir, filepath.Join(toolsDir, "mkfs"))
	require.NoError(t, err)
	require.NoError(t, format(ctx, filepath.Join(t.TempDir(), "vol.img"), "data", 0))
	requireNoTokens(t, envFile)

	toolsDir, envFile = envDumpingMkfs(t)
	seed, err := ResolveVolumeSeeder(ctx, toolsDir, filepath.Join(toolsDir, "mkfs"))
	require.NoError(t, err)
	require.NoError(t, seed(ctx, filepath.Join(t.TempDir(), "vol.img"), "data", 0, "()"))
	requireNoTokens(t, envFile)
}
