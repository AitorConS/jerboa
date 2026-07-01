package pkg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseLddLibs(t *testing.T) {
	out := `	linux-vdso.so.1 (0x00007fffd9dfb000)
	libssl.so.3 => /lib/x86_64-linux-gnu/libssl.so.3 (0x00007f47f9126000)
	libmissing.so.7 => not found
	/lib64/ld-linux-x86-64.so.2 (0x00007f47f950e000)`

	got := parseLddLibs(out)
	require.Equal(t, []string{
		"/lib/x86_64-linux-gnu/libssl.so.3",
		"/lib64/ld-linux-x86-64.so.2",
	}, got, "resolves => paths and the bare interpreter line, skips vdso and 'not found'")
}

func TestFindLoader(t *testing.T) {
	sysroot := t.TempDir()
	loaderRel := filepath.Join("lib64", "ld-linux-x86-64.so.2")
	require.NoError(t, os.MkdirAll(filepath.Join(sysroot, "lib64"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sysroot, loaderRel), []byte("elf"), 0o755))

	// A non-ELF binary path forces the conventional-candidate fallback.
	loader, err := findLoader(sysroot, filepath.Join(t.TempDir(), "not-an-elf"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(sysroot, loaderRel), loader)
}

func TestFindLoader_NotFound(t *testing.T) {
	_, err := findLoader(t.TempDir(), filepath.Join(t.TempDir(), "not-an-elf"))
	require.Error(t, err, "empty sysroot has no dynamic linker")
}

func TestIsUnderDir(t *testing.T) {
	dir := filepath.Clean("/opt/sysroot")
	require.True(t, isUnderDir(dir, filepath.Join(dir, "lib", "libc.so.6")))
	require.True(t, isUnderDir(dir, dir), "the dir itself counts as under it")
	require.False(t, isUnderDir(dir, filepath.Clean("/lib/libc.so.6")), "host path is not under the sysroot")
	require.False(t, isUnderDir(dir, filepath.Clean("/opt/sysroot-evil/lib")), "sibling prefix is not under the sysroot")
}

func TestSysrootLibDirs(t *testing.T) {
	sysroot := t.TempDir()
	// Create a subset of the conventional dirs; only existing ones must be returned.
	require.NoError(t, os.MkdirAll(filepath.Join(sysroot, "lib"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(sysroot, "usr", "lib", "x86_64-linux-gnu"), 0o755))

	dirs := sysrootLibDirs(sysroot)
	require.Contains(t, dirs, filepath.Join(sysroot, "lib"))
	require.Contains(t, dirs, filepath.Join(sysroot, "usr", "lib", "x86_64-linux-gnu"))
	require.NotContains(t, dirs, filepath.Join(sysroot, "lib64"), "non-existent dirs are excluded")
}
