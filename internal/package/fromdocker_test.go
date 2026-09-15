package pkg

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeSyntheticTar builds a tar file describing a container filesystem, without
// creating any real files or symlinks on the host. Regular-file sizes are filled
// in from contents so the archive is well formed.
func writeSyntheticTar(t *testing.T, entries []tar.Header, contents map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "export.tar")
	f, err := os.Create(p)
	require.NoError(t, err)
	tw := tar.NewWriter(f)
	for _, h := range entries {
		hdr := h
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(contents[hdr.Name]))
		}
		require.NoError(t, tw.WriteHeader(&hdr))
		if hdr.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(contents[hdr.Name]))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, f.Close())
	return p
}

func newContainerFS(t *testing.T, tarPath string) *containerFS {
	t.Helper()
	idx, err := indexTar(tarPath)
	require.NoError(t, err)
	return &containerFS{tarPath: tarPath, index: idx}
}

// TestContainerFS_ResolveAndRead is the core F-030 mechanism: reading files out
// of an exported container filesystem, following symlinks logically, with no
// shell/coreutils in the image and no symlinks created on the host.
func TestContainerFS_ResolveAndRead(t *testing.T) {
	tarPath := writeSyntheticTar(t, []tar.Header{
		// tar entry names have no leading slash, matching `docker export`.
		{Name: "usr/local/bin/redis-server", Typeflag: tar.TypeReg, Mode: 0o755},
		// Relative symlink: libc.so.6 -> libc-2.31.so in the same dir.
		{Name: "lib/x86_64-linux-gnu/libc.so.6", Typeflag: tar.TypeSymlink, Linkname: "libc-2.31.so"},
		{Name: "lib/x86_64-linux-gnu/libc-2.31.so", Typeflag: tar.TypeReg, Mode: 0o644},
		// Absolute symlink: /lib64 -> /usr/lib64, then the loader lives under it.
		{Name: "lib64", Typeflag: tar.TypeSymlink, Linkname: "/usr/lib64"},
		{Name: "usr/lib64/ld-linux-x86-64.so.2", Typeflag: tar.TypeReg, Mode: 0o755},
	}, map[string]string{
		"usr/local/bin/redis-server":        "REDIS",
		"lib/x86_64-linux-gnu/libc-2.31.so": "LIBC",
		"usr/lib64/ld-linux-x86-64.so.2":    "LOADER",
	})
	cfs := newContainerFS(t, tarPath)

	// Plain file.
	real, err := cfs.resolve("/usr/local/bin/redis-server")
	require.NoError(t, err)
	require.Equal(t, "/usr/local/bin/redis-server", real)

	// Relative symlink is followed to the real file.
	real, err = cfs.resolve("/lib/x86_64-linux-gnu/libc.so.6")
	require.NoError(t, err)
	require.Equal(t, "/lib/x86_64-linux-gnu/libc-2.31.so", real)
	data, err := cfs.readFile(real)
	require.NoError(t, err)
	require.Equal(t, "LIBC", string(data))

	// Absolute directory-symlink component is followed: /lib64/ld... -> /usr/lib64/ld...
	real, err = cfs.resolve("/lib64/ld-linux-x86-64.so.2")
	require.NoError(t, err)
	require.Equal(t, "/usr/lib64/ld-linux-x86-64.so.2", real)

	// Missing path errors rather than silently returning something.
	_, err = cfs.resolve("/no/such/file")
	require.Error(t, err)
}

func TestContainerFS_ResolveRejectsSymlinkCycle(t *testing.T) {
	tarPath := writeSyntheticTar(t, []tar.Header{
		{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "/b"},
		{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "/a"},
	}, nil)
	cfs := newContainerFS(t, tarPath)
	_, err := cfs.resolve("/a")
	require.Error(t, err)
	require.Contains(t, err.Error(), "symlink hops")
}

// TestElfClosure_NonELF confirms a non-ELF target (e.g. a shell script) yields
// an error: importing a shell launcher cannot produce a runnable package.
func TestElfClosure_NonELF(t *testing.T) {
	tarPath := writeSyntheticTar(t, []tar.Header{
		{Name: "entry.sh", Typeflag: tar.TypeReg, Mode: 0o755},
	}, map[string]string{"entry.sh": "#!/bin/sh\necho hi\n"})
	cfs := newContainerFS(t, tarPath)

	closure, err := cfs.elfClosure("/entry.sh")
	require.Error(t, err)
	require.Nil(t, closure)
}

// TestTarEntryName_NormalizesAndNeutralizesTraversal locks in that guest paths
// are rooted, slash-cleaned, and can never escape the extraction root via "..".
func TestTarEntryName_NormalizesAndNeutralizesTraversal(t *testing.T) {
	cases := map[string]string{
		"/lib64/ld-linux-x86-64.so.2": "lib64/ld-linux-x86-64.so.2",
		"lib64/ld-linux-x86-64.so.2":  "lib64/ld-linux-x86-64.so.2",
		"usr/local/bin/redis-server":  "usr/local/bin/redis-server",
		"libc.so.6":                   "libc.so.6",
		"../../etc/passwd":            "etc/passwd", // ".." neutralized
		"/a/../b/c":                   "b/c",
		"./x":                         "x",
	}
	for in, want := range cases {
		require.Equalf(t, want, tarEntryName(in), "tarEntryName(%q)", in)
	}
	require.Empty(t, tarEntryName("/"))
	require.Empty(t, tarEntryName("."))
}

// TestCreateFromFiles_PreservesGuestPaths is the direct F-024 regression: a
// package built from files at absolute image paths must round-trip through the
// archive and come back with those paths intact — NOT flattened to basenames,
// which is what made from-docker images fail preflight ("interpreter … is not in
// the image"). It exercises CreateFromFiles -> Extract -> ExtractedFileList.
func TestCreateFromFiles_PreservesGuestPaths(t *testing.T) {
	src := t.TempDir()
	mkfile := func(name, content string) string {
		p := filepath.Join(src, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o755))
		return p
	}
	files := []File{
		{HostPath: mkfile("redis-server", "BIN"), GuestPath: "usr/local/bin/redis-server"},
		{HostPath: mkfile("ld.so", "LD"), GuestPath: "lib64/ld-linux-x86-64.so.2"},
		{HostPath: mkfile("libc", "LIBC"), GuestPath: "lib/x86_64-linux-gnu/libc.so.6"},
	}

	store, err := NewStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.CreateFromFiles("redis", "7.2", files, "redis from docker", "redis"))
	require.NoError(t, store.Extract(Package{Name: "redis", Version: "7.2"}))

	got, err := store.ExtractedFileList("redis", "7.2")
	require.NoError(t, err)

	guest := make(map[string]string, len(got))
	for _, f := range got {
		data, rerr := os.ReadFile(f.HostPath)
		require.NoError(t, rerr)
		guest[f.GuestPath] = string(data)
	}
	require.Equal(t, "BIN", guest["usr/local/bin/redis-server"])
	require.Equal(t, "LD", guest["lib64/ld-linux-x86-64.so.2"])
	require.Equal(t, "LIBC", guest["lib/x86_64-linux-gnu/libc.so.6"])
	// The interpreter must live at the exact path a dynamic ELF references, so it
	// is NOT flattened to the image root.
	require.NotContains(t, guest, "ld-linux-x86-64.so.2")
	require.NotContains(t, guest, "redis-server")
}

// TestCreate_FlatLayoutBackwardCompatible confirms the legacy basename-rooted
// Create path is unchanged: a flat archive still yields basename guest paths.
func TestCreate_FlatLayoutBackwardCompatible(t *testing.T) {
	src := t.TempDir()
	bin := filepath.Join(src, "myapp")
	require.NoError(t, os.WriteFile(bin, []byte("BIN"), 0o755))
	lib := filepath.Join(src, "libextra.so")
	require.NoError(t, os.WriteFile(lib, []byte("LIB"), 0o755))

	store, err := NewStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Create("myapp", "1.0.0", bin, []string{lib}, "", ""))
	require.NoError(t, store.Extract(Package{Name: "myapp", Version: "1.0.0"}))

	got, err := store.ExtractedFileList("myapp", "1.0.0")
	require.NoError(t, err)
	guest := map[string]bool{}
	for _, f := range got {
		guest[f.GuestPath] = true
	}
	require.True(t, guest["myapp"])
	require.True(t, guest["libextra.so"])
}
