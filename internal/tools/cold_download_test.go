//go:build integration && linux

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestColdDownloadExecutesRealMkfs(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	require.NoError(t, EnsureKernelTools(ctx, dir))
	mkfs := filepath.Join(dir, "mkfs")
	info, err := os.Stat(mkfs)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	require.NoError(t, os.Chmod(mkfs, 0o644))
	require.NoError(t, EnsureKernelTools(ctx, dir))
	seed, err := ResolveVolumeSeeder(ctx, dir, "")
	require.NoError(t, err)
	disk := filepath.Join(t.TempDir(), "data.img")
	require.NoError(t, seed(ctx, disk, "audit", 4*1024*1024, "(children:())"))
	info, err = os.Stat(disk)
	require.NoError(t, err)
	require.Positive(t, info.Size())
}
