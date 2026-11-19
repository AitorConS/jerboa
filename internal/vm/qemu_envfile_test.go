//go:build linux || (darwin && arm64)

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The guest kernel (kernel/src/unix/env_inject.c) splits opt/uni/env on '\n'
// and each line on its first '='. With "string=" QEMU turned ",," back into ","
// and added no terminator; the file must hold those same bytes, unescaped.
func TestWriteGuestEnvFile_BytesAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env-vm1")
	require.NoError(t, writeGuestEnvFile(path, []string{"A=1", "PATH=/a,/b", "EQ=x=y", "EMPTY="}))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "A=1\nPATH=/a,/b\nEQ=x=y\nEMPTY=", string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// A file or symlink left at the path by an earlier run is replaced, never
// written through: the secrets must not land wherever a stale link points.
func TestWriteGuestEnvFile_ReplacesStaleSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	require.NoError(t, os.WriteFile(target, []byte("untouched"), 0o644))
	path := filepath.Join(dir, "env-vm1")
	require.NoError(t, os.Symlink(target, path))

	require.NoError(t, writeGuestEnvFile(path, []string{"K=v"}))
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "untouched", string(data))
}

func TestWriteGuestEnvFile_UnwritableDirFails(t *testing.T) {
	err := writeGuestEnvFile(filepath.Join(t.TempDir(), "missing", "env-vm1"), []string{"K=v"})
	require.ErrorContains(t, err, "write guest environment")
}
