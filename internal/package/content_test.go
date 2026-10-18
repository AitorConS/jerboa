package pkg

import (
	"archive/tar"
	"compress/gzip"
	"debug/elf"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// packageTree writes a small package tree under root with the given file
// modes and modification time, and returns its files with the program first.
func packageTree(t *testing.T, root string, binMode, dataMode os.FileMode, mtime time.Time) []File {
	t.Helper()
	write := func(rel string, data []byte, mode os.FileMode) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, data, mode))
		require.NoError(t, os.Chmod(p, mode))
		require.NoError(t, os.Chtimes(p, mtime, mtime))
		return p
	}
	bin := write("bin/app", syntheticELF(elf.EM_X86_64, "", nil, ""), binMode)
	return []File{
		{HostPath: bin, GuestPath: "/usr/local/bin/app"},
		{HostPath: write("share/b.txt", []byte("bee"), dataMode), GuestPath: "/usr/share/app/b.txt"},
		{HostPath: write("share/a.txt", []byte("ay"), dataMode), GuestPath: "/usr/share/app/a.txt"},
	}
}

func readMeta(t *testing.T, s *Store, name, version string) Package {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.PackageDir(name, version), "meta.json"))
	require.NoError(t, err)
	var p Package
	require.NoError(t, json.Unmarshal(data, &p))
	return p
}

// The same files give a byte-identical archive regardless of file order,
// timestamps, permission bits beyond executable, or owner.
func TestCreateFromFiles_ReproducibleArchive(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")

	filesA := packageTree(t, t.TempDir(), 0o755, 0o644, time.Unix(1, 0))
	filesB := packageTree(t, t.TempDir(), 0o700, 0o600, time.Now())
	filesB[1], filesB[2] = filesB[2], filesB[1]

	storeA, err := NewStore(t.TempDir())
	require.NoError(t, err)
	storeB, err := NewStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, storeA.CreateFromFiles("app", "1.0.0", filesA, "", ""))
	require.NoError(t, storeB.CreateFromFiles("app", "1.0.0", filesB, "", ""))

	metaA, metaB := readMeta(t, storeA, "app", "1.0.0"), readMeta(t, storeB, "app", "1.0.0")
	require.Equal(t, metaA.SHA256, metaB.SHA256)
	require.Equal(t, metaA.ContentDigest, metaB.ContentDigest)
	require.True(t, ValidContentDigest(metaA.ContentDigest))
	require.Equal(t, time.Unix(1700000000, 0).UTC(), metaA.Created)

	f, err := os.Open(filepath.Join(storeA.PackageDir("app", "1.0.0"), "files.tar.gz"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
		require.Zero(t, hdr.Uid)
		require.Zero(t, hdr.Gid)
		require.Empty(t, hdr.Uname)
		require.Empty(t, hdr.Gname)
		require.Equal(t, int64(1700000000), hdr.ModTime.Unix())
		if hdr.Name == "usr/local/bin/app" {
			require.Equal(t, int64(0o755), hdr.Mode)
		} else {
			require.Equal(t, int64(0o644), hdr.Mode)
		}
	}
	require.Equal(t, []string{"usr/local/bin/app", "usr/share/app/a.txt", "usr/share/app/b.txt"}, names)
}

// The content digest does not depend on compression, so it survives a repack,
// and Verify reports it and rejects files that no longer match it.
func TestVerify_ContentDigest(t *testing.T) {
	files := packageTree(t, t.TempDir(), 0o755, 0o644, time.Now())
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.CreateFromFiles("app", "1.0.0", files, "", ""))
	meta := readMeta(t, store, "app", "1.0.0")

	direct, err := ContentDigest(files)
	require.NoError(t, err)
	require.Equal(t, meta.ContentDigest, direct)

	_, ref, err := store.Verify(meta, "linux/amd64")
	require.NoError(t, err)
	require.Equal(t, meta.ContentDigest, ref.ContentDigest)
	require.NoError(t, ValidateReferences([]Reference{ref}, "linux/amd64"))

	tampered := meta
	tampered.ContentDigest = ContentDigestPrefix + "00000000000000000000000000000000000000000000000000000000000000aa"
	_, _, err = store.Verify(tampered, "linux/amd64")
	require.ErrorIs(t, err, ErrContentMismatch)

	bad := ref
	bad.ContentDigest = "md5:abc"
	require.Error(t, ValidateReferences([]Reference{bad}, "linux/amd64"))
}

// Only the executable bit and the bytes of each file at its guest path count.
func TestContentDigest_Inputs(t *testing.T) {
	root := t.TempDir()
	write := func(name, data string, mode os.FileMode) string {
		p := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(p, []byte(data), mode))
		require.NoError(t, os.Chmod(p, mode))
		return p
	}
	a := write("a", "same", 0o644)
	b := write("b", "same", 0o600)
	x := write("x", "same", 0o755)

	base, err := ContentDigest([]File{{HostPath: a, GuestPath: "/f"}})
	require.NoError(t, err)
	same, err := ContentDigest([]File{{HostPath: b, GuestPath: "f"}, {GuestPath: "dir", IsDir: true}})
	require.NoError(t, err)
	require.Equal(t, base, same)

	exec, err := ContentDigest([]File{{HostPath: x, GuestPath: "/f"}})
	require.NoError(t, err)
	require.NotEqual(t, base, exec)
	moved, err := ContentDigest([]File{{HostPath: a, GuestPath: "/g"}})
	require.NoError(t, err)
	require.NotEqual(t, base, moved)
}

func TestCreateFromFiles_RejectsDuplicateGuestPath(t *testing.T) {
	files := packageTree(t, t.TempDir(), 0o755, 0o644, time.Now())
	files[2].GuestPath = files[1].GuestPath
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)
	require.ErrorContains(t, store.CreateFromFiles("app", "1.0.0", files, "", ""), "listed twice")
}
