package pkg

import (
	"debug/elf"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsRuntimeBloat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rel  string
		want bool
	}{
		{"usr/local/lib/python3.12/test/test_os.py", true},
		{"usr/local/lib/python3.12/idlelib/idle.py", true},
		{"usr/local/lib/python3.12/tkinter/__init__.py", true},
		{"usr/local/lib/python3.12/turtledemo/clock.py", true},
		{"usr/local/lib/python3.12/ensurepip/_bundled/pip.whl", true},
		{"usr/local/lib/python3.12/config-3.12-x86_64-linux-gnu/Makefile", true},
		{"usr/local/lib/python3.12/__pycache__/os.cpython-312.opt-1.pyc", true},
		{"usr/local/lib/python3.12/json/__pycache__/decoder.cpython-312.opt-2.pyc", true},
		{"usr/local/lib/python3.12/__pycache__/os.cpython-312.pyc", false},
		{"usr/local/lib/python3.12/os.py", false},
		{"usr/local/lib/python3.12/site-packages/foo/test/test_foo.py", false},
		{"usr/local/lib/python3.12/unittest/__init__.py", false},
		{"app/pythonista/test/fixture.txt", false},
		{"opt/python/test/data.txt", false},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, isRuntimeBloat(tt.rel), tt.rel)
	}
}

// Creating a package from a full interpreter install leaves the build-time
// subtrees out of the archive.
func TestCreateFromFiles_PrunesRuntimeBloat(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, data []byte) File {
		p := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, data, 0o755))
		return File{HostPath: p, GuestPath: "/" + rel}
	}
	files := []File{
		write("usr/local/bin/python3", syntheticELF(elf.EM_X86_64, "", nil, "")),
		write("usr/local/lib/python3.12/os.py", []byte("os")),
		write("usr/local/lib/python3.12/__pycache__/os.cpython-312.pyc", []byte("pyc")),
		write("usr/local/lib/python3.12/__pycache__/os.cpython-312.opt-1.pyc", []byte("opt1")),
		write("usr/local/lib/python3.12/test/test_os.py", []byte("test")),
		write("usr/local/lib/python3.12/ensurepip/__init__.py", []byte("pip")),
		{GuestPath: "/usr/local/lib/python3.12/idlelib", IsDir: true},
	}
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.CreateFromFiles("python", "3.12.0", files, "", ""))
	require.NoError(t, store.Extract(Package{Name: "python", Version: "3.12.0"}))
	got, err := store.ExtractedFileList("python", "3.12.0")
	require.NoError(t, err)
	var names []string
	for _, f := range got {
		names = append(names, f.GuestPath)
	}
	require.ElementsMatch(t, []string{
		"usr/local/bin/python3",
		"usr/local/lib/python3.12/os.py",
		"usr/local/lib/python3.12/__pycache__/os.cpython-312.pyc",
	}, names)
	_, err = os.Stat(filepath.Join(store.PackageDir("python", "3.12.0"), "files", "usr/local/lib/python3.12/idlelib"))
	require.True(t, os.IsNotExist(err))
}
