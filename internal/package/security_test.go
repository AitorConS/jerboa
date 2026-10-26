package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// maliciousRefs are package coordinates a compromised remote index could
// return to escape the store root. Every store method that turns a coordinate
// into a filesystem path must reject them before touching disk.
var maliciousRefs = []struct {
	name  string
	value string
}{
	{"parent traversal", "../evil"},
	{"deep traversal", "../../../.ssh"},
	{"absolute-ish", "/etc/passwd"},
	{"backslash", `..\evil`},
	{"embedded dotdot", "a/../../b"},
	{"empty", ""},
}

func TestStore_RejectsMaliciousRefs(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	// A sentinel sibling of the store root that a traversal would try to reach.
	outside := filepath.Join(filepath.Dir(root), "SENTINEL")
	require.NoError(t, os.WriteFile(outside, []byte("do not touch"), 0o644))
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, m := range maliciousRefs {
		t.Run(m.name, func(t *testing.T) {
			// name field poisoned.
			require.Error(t, store.Download(Package{Name: m.value, Version: "1.0.0", URL: "http://127.0.0.1:0"}))
			require.Error(t, store.Extract(Package{Name: m.value, Version: "1.0.0"}))
			require.Error(t, store.SaveMeta(Package{Name: m.value, Version: "1.0.0"}))
			require.Error(t, store.Remove(m.value, "1.0.0"))
			require.Error(t, store.RemoveAll(m.value))
			require.Error(t, store.Create(m.value, "1.0.0", "bin", nil, "", ""))
			require.False(t, store.IsDownloaded(m.value, "1.0.0"))
			require.False(t, store.IsExtracted(m.value, "1.0.0"))
			_, err := store.ExtractedFiles(m.value, "1.0.0")
			require.Error(t, err)

			// version field poisoned.
			require.Error(t, store.Download(Package{Name: "node", Version: m.value, URL: "http://127.0.0.1:0"}))
			require.Error(t, store.Remove("node", m.value))
		})
	}

	data, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "do not touch", string(data), "traversal must not overwrite files outside the store root")
}

func TestOpsStore_RejectsMaliciousRefs(t *testing.T) {
	root := t.TempDir()
	store, err := NewOpsStore(root)
	require.NoError(t, err)

	outside := filepath.Join(filepath.Dir(root), "SENTINEL_OPS")
	require.NoError(t, os.WriteFile(outside, []byte("do not touch"), 0o644))
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, m := range maliciousRefs {
		t.Run(m.name, func(t *testing.T) {
			// Each of the three coordinates must be validated independently.
			require.Error(t, store.Download(m.value, "node", "v1.0.0", ""))
			require.Error(t, store.Download("eyberg", m.value, "v1.0.0", ""))
			require.Error(t, store.Download("eyberg", "node", m.value, ""))

			require.Error(t, store.Extract(m.value, "node", "v1.0.0"))
			require.Error(t, store.Remove(m.value, "node", "v1.0.0"))
			require.False(t, store.IsDownloaded(m.value, "node", "v1.0.0"))
			require.False(t, store.IsExtracted(m.value, "node", "v1.0.0"))

			_, err := store.ExtractedFiles(m.value, "node", "v1.0.0")
			require.Error(t, err)
			_, err = store.FindBinary(m.value, "node", "v1.0.0")
			require.Error(t, err)
			_, err = store.LoadPackageManifest(m.value, "node", "v1.0.0")
			require.Error(t, err)
		})
	}

	data, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "do not touch", string(data), "traversal must not overwrite files outside the store root")
}

// TestStore_Extract_RejectsTraversal checks the Zip Slip guard: an entry whose
// name climbs out of the extraction directory aborts the extraction instead of
// writing next to it. Rooted names are re-rooted by filepath.Join and stay in.
func TestStore_Extract_RejectsTraversal(t *testing.T) {
	for _, entry := range []string{"../escaped", "sub/../../escaped", "a/b/../../../escaped"} {
		t.Run(entry, func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewStore(dir)
			require.NoError(t, err)

			pkgDir := store.PackageDir("evilpkg", "1.0.0")
			require.NoError(t, os.MkdirAll(pkgDir, 0o755))

			f, err := os.Create(filepath.Join(pkgDir, "files.tar.gz"))
			require.NoError(t, err)
			gw := gzip.NewWriter(f)
			tw := tar.NewWriter(gw)
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name: entry, Typeflag: tar.TypeReg, Size: 5, Mode: 0o644,
			}))
			_, err = tw.Write([]byte("pwned"))
			require.NoError(t, err)
			require.NoError(t, tw.Close())
			require.NoError(t, gw.Close())
			require.NoError(t, f.Close())

			err = store.Extract(Package{Name: "evilpkg", Version: "1.0.0"})
			require.Error(t, err)
			require.Contains(t, err.Error(), "escapes extraction directory")
			_, statErr := os.Stat(filepath.Join(pkgDir, "escaped"))
			require.True(t, os.IsNotExist(statErr), "traversing entry must not be written")
		})
	}
}

// TestExtract_SymlinkEscape covers the full archive path: a symlink pointing
// outside the package dir followed by an entry writing "through" it. The entry
// name stays lexically inside the package, so only the link check and the
// symlink-aware path walk keep the write contained.
func TestExtract_SymlinkEscape(t *testing.T) {
	root := t.TempDir()
	store, err := NewOpsStore(root)
	require.NoError(t, err)

	dir := store.PackageDir("evil", "pkg", "1.0")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	outsideDir := t.TempDir()
	victim := filepath.Join(outsideDir, "authorized_keys")
	require.NoError(t, os.WriteFile(victim, []byte("original"), 0o600))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "pkg/", Typeflag: tar.TypeDir, Mode: 0o755,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "pkg/sysroot", Typeflag: tar.TypeSymlink, Linkname: outsideDir, Mode: 0o777,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "pkg/sysroot/authorized_keys", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len("pwned")),
	}))
	_, err = tw.Write([]byte("pwned"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	require.NoError(t, os.WriteFile(filepath.Join(dir, ArchSlug()+".tar.gz"), buf.Bytes(), 0o644))
	require.NoError(t, store.Extract("evil", "pkg", "1.0"))

	got, err := os.ReadFile(victim)
	require.NoError(t, err)
	require.Equal(t, "original", string(got), "extraction must not write through an escaping symlink")
}

// TestTraversesSymlink covers the second layer of the extraction guard: even if
// a link were created inside the package dir, an entry whose path crosses it is
// refused, because os.OpenFile and os.MkdirAll would follow it off the disk.
func TestTraversesSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sysroot", "lib"), 0o755))
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	require.False(t, traversesSymlink(dir, filepath.Join(dir, "sysroot", "lib", "libc.so")))
	require.False(t, traversesSymlink(dir, filepath.Join(dir, "not", "created", "yet")))
	require.True(t, traversesSymlink(dir, filepath.Join(dir, "escape")))
	require.True(t, traversesSymlink(dir, filepath.Join(dir, "escape", "authorized_keys")))
}

// TestResolvesWithinDir covers the EvalSymlinks-based containment check used
// before creating a symlink. It must follow real links (a path leading through
// an escaping link is rejected), tolerate a not-yet-extracted trailing
// component, and keep ordinary in-tree paths.
func TestResolvesWithinDir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sysroot", "lib"), 0o755))
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	// Plain in-tree paths stay contained, whether or not the leaf exists yet.
	require.True(t, resolvesWithinDir(dir, filepath.Join(dir, "sysroot", "lib")))
	require.True(t, resolvesWithinDir(dir, filepath.Join(dir, "sysroot", "lib", "libc.so.6")))
	require.True(t, resolvesWithinDir(dir, filepath.Join(dir, "not", "created", "yet")))

	// A path whose parent is an escaping symlink resolves outside dir.
	require.False(t, resolvesWithinDir(dir, filepath.Join(dir, "escape")))
	require.False(t, resolvesWithinDir(dir, filepath.Join(dir, "escape", "authorized_keys")))

	// evalSymlinksLenient resolves the longest existing prefix and re-appends the
	// missing tail rather than failing outright.
	resolved, err := evalSymlinksLenient(filepath.Join(dir, "sysroot", "lib", "does", "not", "exist"))
	require.NoError(t, err)
	realDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.True(t, withinDir(realDir, resolved))
}

// TestStore_Extract_CleansUpAfterFailure checks that an aborted extraction
// leaves nothing behind. IsExtracted only tests that files/ is non-empty, so a
// half-written tree would be served to the next caller as if it were complete.
func TestStore_Extract_CleansUpAfterFailure(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("evilpkg", "1.0.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	f, err := os.Create(filepath.Join(pkgDir, "files.tar.gz"))
	require.NoError(t, err)
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	// A legitimate entry lands on disk before the traversing one aborts the run.
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "bin/app", Typeflag: tar.TypeReg, Size: 2, Mode: 0o755,
	}))
	_, err = tw.Write([]byte("ok"))
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "../escaped", Typeflag: tar.TypeReg, Size: 5, Mode: 0o644,
	}))
	_, err = tw.Write([]byte("pwned"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	require.NoError(t, f.Close())

	require.Error(t, store.Extract(Package{Name: "evilpkg", Version: "1.0.0"}))

	_, statErr := os.Stat(filepath.Join(pkgDir, "files"))
	require.True(t, os.IsNotExist(statErr), "partial extraction must be discarded")
	require.False(t, store.IsExtracted("evilpkg", "1.0.0"))
}

// TestOpsStore_Extract_DecompressionBudget checks the cap on uncompressed size
// and, with it, that a failed ops extraction is discarded while the downloaded
// archive is kept so a retry does not have to fetch it again.
func TestOpsStore_Extract_DecompressionBudget(t *testing.T) {
	orig := maxExtractedBytes
	maxExtractedBytes = 32
	t.Cleanup(func() { maxExtractedBytes = orig })

	root := t.TempDir()
	store, err := NewOpsStore(root)
	require.NoError(t, err)

	pkgDir := store.PackageDir("eyberg", "fat", "1.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	archive := createOpsPackageArchive(t, map[string]string{
		"package.manifest": `{"Program":"fat"}`,
		"fat":              strings.Repeat("A", 128),
	})
	archivePath := filepath.Join(pkgDir, ArchSlug()+".tar.gz")
	require.NoError(t, os.WriteFile(archivePath, archive, 0o644))

	err = store.Extract("eyberg", "fat", "1.0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "expands beyond")

	require.False(t, store.IsExtracted("eyberg", "fat", "1.0"))
	_, statErr := os.Stat(archivePath)
	require.NoError(t, statErr, "the downloaded archive must survive the cleanup")
}

// TestExtract_ReadOnlyDirEntry checks that a read-only directory entry does not
// abort the extraction: the archive controls the mode, and 0o555 would make
// every write beneath it fail with EACCES.
func TestExtract_ReadOnlyDirEntry(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("ropkg", "1.0.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	f, err := os.Create(filepath.Join(pkgDir, "files.tar.gz"))
	require.NoError(t, err)
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "locked", Typeflag: tar.TypeDir, Mode: 0o555,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "locked/app", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644,
	}))
	_, err = tw.Write([]byte("hi"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	require.NoError(t, f.Close())

	require.NoError(t, store.Extract(Package{Name: "ropkg", Version: "1.0.0"}))

	files, err := store.ExtractedFiles("ropkg", "1.0.0")
	require.NoError(t, err)
	require.Len(t, files, 1)
}

// TestStore_Extract_RejectsLinks checks that a first-party package archive with
// a symlink or hard link fails loudly instead of extracting without it.
func TestStore_Extract_RejectsLinks(t *testing.T) {
	for _, typ := range []byte{tar.TypeSymlink, tar.TypeLink} {
		t.Run(string(typ), func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewStore(dir)
			require.NoError(t, err)
			pkgDir := store.PackageDir("linkpkg", "1.0.0")
			require.NoError(t, os.MkdirAll(pkgDir, 0o755))

			f, err := os.Create(filepath.Join(pkgDir, "files.tar.gz"))
			require.NoError(t, err)
			gw := gzip.NewWriter(f)
			tw := tar.NewWriter(gw)
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: "lib/libz.so.1.3", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644}))
			_, err = tw.Write([]byte("zz"))
			require.NoError(t, err)
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: "lib/libz.so.1", Typeflag: typ, Linkname: "lib/libz.so.1.3", Mode: 0o777}))
			require.NoError(t, tw.Close())
			require.NoError(t, gw.Close())
			require.NoError(t, f.Close())

			err = store.Extract(Package{Name: "linkpkg", Version: "1.0.0"})
			require.ErrorContains(t, err, "unsupported entry")
			require.False(t, store.IsExtracted("linkpkg", "1.0.0"), "a failed extraction must not look complete")
		})
	}
}

// TestExtract_SymlinkClimbingThroughALink covers a link name that stays inside
// the package once cleaned but climbs out on disk: an earlier entry plants
// "s -> .", so each "s/" stays in place while each ".." really climbs.
func TestExtract_SymlinkClimbingThroughALink(t *testing.T) {
	root := t.TempDir()
	store, err := NewOpsStore(root)
	require.NoError(t, err)
	dir := store.PackageDir("evil", "pkg", "1.0")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	secret := filepath.Join(t.TempDir(), "id_rsa")
	require.NoError(t, os.WriteFile(secret, []byte("host secret"), 0o600))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, d := range []string{"pkg/", "pkg/sysroot/", "pkg/sysroot/etc/", "pkg/sysroot/test/"} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755}))
	}
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "pkg/sysroot/test/s", Typeflag: tar.TypeSymlink, Linkname: ".", Mode: 0o777}))
	link := "../test/" + strings.Repeat("s/", 20) + strings.Repeat("../", 20) + strings.TrimPrefix(secret, "/")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "pkg/sysroot/etc/leak", Typeflag: tar.TypeSymlink, Linkname: link, Mode: 0o777}))
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, ArchSlug()+".tar.gz"), buf.Bytes(), 0o644))
	require.NoError(t, store.Extract("evil", "pkg", "1.0"))

	files, err := store.ExtractedFiles("evil", "pkg", "1.0")
	require.NoError(t, err)
	for _, f := range files {
		if f.IsDir {
			continue
		}
		data, err := os.ReadFile(f.HostPath)
		require.NoError(t, err)
		require.NotContains(t, string(data), "host secret", f.GuestPath)
	}
}

// TestExtractedFiles_SkipsEscapingSymlinks covers the last layer: whatever
// extraction let through, a link on disk is listed only by what it resolves
// to, and only when that is a regular file inside the package.
func TestExtractedFiles_SkipsEscapingSymlinks(t *testing.T) {
	store, err := NewOpsStore(t.TempDir())
	require.NoError(t, err)
	dir := store.PackageDir("evil", "pkg", "1.0")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sysroot", "lib"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sysroot", "lib", "libz.so.1.3"), []byte("lib"), 0o644))
	secret := filepath.Join(t.TempDir(), "id_rsa")
	require.NoError(t, os.WriteFile(secret, []byte("host secret"), 0o600))
	if err := os.Symlink(secret, filepath.Join(dir, "sysroot", "lib", "leak")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	require.NoError(t, os.Symlink("libz.so.1.3", filepath.Join(dir, "sysroot", "lib", "libz.so.1")))

	files, err := store.ExtractedFiles("evil", "pkg", "1.0")
	require.NoError(t, err)
	got := map[string]string{}
	for _, f := range files {
		got[f.GuestPath] = f.HostPath
	}
	require.NotContains(t, got, "lib/leak")
	require.Contains(t, got, "lib/libz.so.1")
	data, err := os.ReadFile(got["lib/libz.so.1"])
	require.NoError(t, err)
	require.Equal(t, "lib", string(data))
}

func TestPhysicallyWithin(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "sysroot", "lib")
	require.NoError(t, os.MkdirAll(base, 0o755))
	if err := os.Symlink(".", filepath.Join(base, "s")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	require.True(t, physicallyWithin(dir, base, "libc.so.6"))
	require.True(t, physicallyWithin(dir, base, "../share/x"))
	require.True(t, physicallyWithin(dir, base, "s/libc.so.6"))
	require.False(t, physicallyWithin(dir, base, "s/../x"), "a .. after a link climbs from the link's target")
	require.False(t, physicallyWithin(dir, base, "../../../x"))
}
