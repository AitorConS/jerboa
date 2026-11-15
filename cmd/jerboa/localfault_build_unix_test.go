//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/AitorConS/jerboa/internal/builder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A source tree is packaged file by file. Links that loop, dangle or lead
// out of the project, and entries that are not files, must stop the build
// with the entry named: following them would hang the build or put files
// from outside the project into the image.
func TestLocalFault_SourceTreeLinks(t *testing.T) {
	newProject := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		lfWrite(t, filepath.Join(dir, "main.py"), "print('hi')")
		lfWrite(t, filepath.Join(dir, "lib", "util.py"), "x = 1")
		return dir
	}
	bad := map[string]func(t *testing.T, dir string){
		"link to itself": func(t *testing.T, dir string) {
			require.NoError(t, os.Symlink(filepath.Join(dir, "loop"), filepath.Join(dir, "loop")))
		},
		"two links in a loop": func(t *testing.T, dir string) {
			require.NoError(t, os.Symlink(filepath.Join(dir, "b"), filepath.Join(dir, "a")))
			require.NoError(t, os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "b")))
		},
		"dangling link": func(t *testing.T, dir string) {
			require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "dangling")))
		},
		"link out of the project": func(t *testing.T, dir string) {
			secret := lfWrite(t, filepath.Join(t.TempDir(), "id_rsa"), "PRIVATE KEY")
			require.NoError(t, os.Symlink(secret, filepath.Join(dir, "key")))
		},
		"relative link out of the project": func(t *testing.T, dir string) {
			lfWrite(t, filepath.Join(filepath.Dir(dir), "outside.txt"), "outside")
			require.NoError(t, os.Symlink("../outside.txt", filepath.Join(dir, "lib", "out")))
		},
		"link to a directory": func(t *testing.T, dir string) {
			require.NoError(t, os.Symlink(filepath.Join(dir, "lib"), filepath.Join(dir, "liblink")))
		},
		"link to the project itself": func(t *testing.T, dir string) {
			require.NoError(t, os.Symlink(dir, filepath.Join(dir, "lib", "self")))
		},
		"named pipe": func(t *testing.T, dir string) {
			require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600))
		},
	}
	for name, setup := range bad {
		t.Run(name, func(t *testing.T) {
			dir := newProject(t)
			setup(t, dir)
			files, err := sourceFiles(dir, builder.LangPython, nil)
			require.Error(t, err, "packaged: %+v", files)
			assert.Contains(t, err.Error(), "unsupported")
			assert.Nil(t, files)
		})
	}

	t.Run("link to a project file is packaged as that file", func(t *testing.T) {
		dir := newProject(t)
		require.NoError(t, os.Symlink(filepath.Join(dir, "lib", "util.py"), filepath.Join(dir, "alias.py")))
		files, err := sourceFiles(dir, builder.LangPython, nil)
		require.NoError(t, err)
		guests := map[string]bool{}
		for _, f := range files {
			guests[f.GuestPath] = true
			assert.True(t, f.FromContext)
		}
		assert.Equal(t, map[string]bool{"main.py": true, "lib/util.py": true, "alias.py": true}, guests)
	})

	t.Run("an ignored directory is not even looked into", func(t *testing.T) {
		dir := newProject(t)
		lfWrite(t, filepath.Join(dir, ".unignore"), "scratch/\n")
		require.NoError(t, os.Mkdir(filepath.Join(dir, "scratch"), 0o700))
		require.NoError(t, os.Symlink(filepath.Join(dir, "scratch", "loop"), filepath.Join(dir, "scratch", "loop")))
		require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "scratch", "pipe"), 0o600))
		files, err := sourceFiles(dir, builder.LangPython, nil)
		require.NoError(t, err)
		for _, f := range files {
			assert.NotContains(t, f.GuestPath, "scratch")
		}
	})
}
