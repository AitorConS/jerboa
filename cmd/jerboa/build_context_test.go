//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/builder"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The build context is listed before the build steps finish and streamed
// after them. A step (or a process it left behind) that replaces a listed
// file in between must not get a host file streamed into the image in its
// place: the stream fails and the file's contents never reach the archive.
func TestBuildContextRefusesFilesChangedAfterTheWalk(t *testing.T) {
	const secret = "registry-token-12345"
	newProject := func(t *testing.T) (dir, secretPath string) {
		t.Helper()
		dir = t.TempDir()
		lfWrite(t, filepath.Join(dir, "main.py"), "print('hi')")
		lfWrite(t, filepath.Join(dir, "lib", "util.py"), "x = 1")
		lfWrite(t, filepath.Join(dir, "lib", "other.py"), "y = 2")
		require.NoError(t, os.Symlink("util.py", filepath.Join(dir, "lib", "alias.py")))
		return dir, lfWrite(t, filepath.Join(t.TempDir(), "credentials.json"), secret)
	}
	cases := map[string]func(t *testing.T, dir, secretPath string){
		"file swapped for a link to a host file": func(t *testing.T, dir, secretPath string) {
			p := filepath.Join(dir, "main.py")
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Symlink(secretPath, p))
		},
		"file swapped for a link inside the project": func(t *testing.T, dir, _ string) {
			p := filepath.Join(dir, "main.py")
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Symlink("lib/other.py", p))
		},
		"file replaced by a hard link to a host file": func(t *testing.T, dir, secretPath string) {
			p := filepath.Join(dir, "main.py")
			require.NoError(t, os.Remove(p))
			if err := os.Link(secretPath, p); err != nil {
				t.Skipf("hard links across these directories: %v", err)
			}
		},
		"listed link pointed out of the project": func(t *testing.T, dir, secretPath string) {
			p := filepath.Join(dir, "lib", "alias.py")
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Symlink(secretPath, p))
		},
		"listed link's target swapped for a link out of the project": func(t *testing.T, dir, secretPath string) {
			p := filepath.Join(dir, "lib", "util.py")
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Symlink(secretPath, p))
		},
		"directory swapped for a link out of the project": func(t *testing.T, dir, secretPath string) {
			lib := filepath.Join(dir, "lib")
			outside := filepath.Dir(secretPath)
			lfWrite(t, filepath.Join(outside, "util.py"), secret)
			lfWrite(t, filepath.Join(outside, "other.py"), secret)
			lfWrite(t, filepath.Join(outside, "alias.py"), secret)
			require.NoError(t, os.RemoveAll(lib))
			require.NoError(t, os.Symlink(outside, lib))
		},
	}
	for name, swap := range cases {
		t.Run(name, func(t *testing.T) {
			dir, secretPath := newProject(t)
			files, err := sourceFiles(dir, builder.LangPython, nil)
			require.NoError(t, err)
			swap(t, dir, secretPath)

			r := buildContextReader(pkg.File{HostPath: lfWrite(t, filepath.Join(t.TempDir(), "python3"), "runtime")}, "", files)
			defer func() { _ = r.Close() }()
			entries, err := lfTarEntries(r)
			require.Error(t, err)
			assert.Contains(t, err.Error(), dir, "the error names the file")
			for name, content := range entries {
				assert.NotContains(t, content, secret, "%s carries the host file", name)
			}
		})
	}
}

// Files and in-project links left alone are streamed as before.
func TestBuildContextStreamsUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	lfWrite(t, filepath.Join(dir, "main.py"), "print('hi')")
	lfWrite(t, filepath.Join(dir, "lib", "util.py"), "x = 1")
	require.NoError(t, os.Symlink("util.py", filepath.Join(dir, "lib", "alias.py")))
	files, err := sourceFiles(dir, builder.LangPython, nil)
	require.NoError(t, err)

	r := buildContextReader(pkg.File{HostPath: lfWrite(t, filepath.Join(t.TempDir(), "python3"), "runtime")}, "", files)
	defer func() { _ = r.Close() }()
	entries, err := lfTarEntries(r)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		buildProgramPath: "runtime",
		"main.py":        "print('hi')",
		"lib/util.py":    "x = 1",
		"lib/alias.py":   "x = 1",
	}, entries)
}

// A build output checked after the build is streamed only if it is still
// that file.
func TestBuildContextRefusesAReplacedBuildOutput(t *testing.T) {
	dir := t.TempDir()
	out := lfWrite(t, filepath.Join(dir, ".jerboa-build-binary"), "elf")
	secretPath := lfWrite(t, filepath.Join(t.TempDir(), "credentials.json"), "secret")
	walked, err := os.Lstat(out)
	require.NoError(t, err)

	require.NoError(t, os.Remove(out))
	require.NoError(t, os.Symlink(secretPath, out))
	r := buildContextReader(pkg.File{HostPath: out, Walked: walked}, "", nil)
	defer func() { _ = r.Close() }()
	entries, err := lfTarEntries(r)
	require.Error(t, err)
	for name, content := range entries {
		require.NotContains(t, content, "secret", name)
	}
}
