package tools

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildNanosManifest(t *testing.T) {
	manifest := buildNanosManifest("/abs/path/app")
	require.Contains(t, manifest, "program:(contents:(host:/abs/path/app))")
	require.Contains(t, manifest, "program:/program")
	require.Contains(t, manifest, "environment:()")
}

func TestDirectFunc_DefaultManifest(t *testing.T) {
	mkfs := directFunc("/bin/mkfs", "/tmp/boot.img", "/tmp/kernel.img")
	cmd := mkfs(context.Background(), "/tmp/disk.img", "relative/app", "")

	require.Equal(t, "/bin/mkfs", cmd.Path)
	require.Contains(t, cmd.Args, "-b")
	require.Contains(t, cmd.Args, "/tmp/boot.img")
	require.Contains(t, cmd.Args, "-k")
	require.Contains(t, cmd.Args, "/tmp/kernel.img")
	require.Contains(t, cmd.Args, "/tmp/disk.img")

	data, err := io.ReadAll(cmd.Stdin)
	require.NoError(t, err)
	require.Contains(t, string(data), "program:(contents:(host:")
}

func TestDirectFunc_CustomManifest(t *testing.T) {
	mkfs := directFunc("/bin/mkfs", "/tmp/boot.img", "/tmp/kernel.img")
	cmd := mkfs(context.Background(), "/tmp/disk.img", "bin", "custom-manifest")

	data, err := io.ReadAll(cmd.Stdin)
	require.NoError(t, err)
	require.Equal(t, "custom-manifest", string(data))
}

func TestResolveMkfs_UsesExistingToolsDir(t *testing.T) {
	toolsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "mkfs"), []byte("x"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "kernel.img"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "boot.img"), []byte("x"), 0o644))

	if runtime.GOOS == "windows" {
		_, err := ResolveMkfs(context.Background(), toolsDir, "")
		if err != nil {
			t.Skipf("WSL not available on this host: %v", err)
		}
		return
	}

	mkfs, err := ResolveMkfs(context.Background(), toolsDir, "")
	require.NoError(t, err)
	require.NotNil(t, mkfs)

	cmd := mkfs(context.Background(), "/tmp/disk.img", "app", "")
	require.Equal(t, filepath.Join(toolsDir, "mkfs"), cmd.Path)
}
