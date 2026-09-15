package pkg

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOpsRuntimeBloatDir pins the compile-time-only subtrees that must be
// pruned from the runtime image. The function keys off a "python*" path
// component followed by a known build directory, so the cases cover each
// excluded name, a non-python neighbor, and paths where "python" is the
// trailing component (nothing to exclude).
func TestOpsRuntimeBloatDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rel  string
		want bool
	}{
		{"config subtree", "lib/python3.12/config-3.12-x86_64-linux-gnu", true},
		{"test subtree", "lib/python3.12/test", true},
		{"lib2to3 subtree", "lib/python3.12/lib2to3", true},
		{"pydoc_data subtree", "lib/python3.12/pydoc_data", true},
		{"nested under sysroot", "sysroot/lib/python3.11/test", true},
		{"runtime module kept", "lib/python3.12/os.py", false},
		{"site-packages kept", "lib/python3.12/site-packages", false},
		{"python is trailing component", "lib/python3.12", false},
		{"non-python config dir kept", "lib/ssl/config-1.1", false},
		{"unrelated path", "etc/ssl/certs", false},
		{"empty path", "", false},
		{"config prefix on python dir itself", "python3.12/config-x", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, opsRuntimeBloatDir(tt.rel))
		})
	}
}

// writePkgFile creates a file (and any parent directories) under root using a
// slash-separated relative path, so tests can lay out a package tree portably.
func writePkgFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

// TestOpsStore_ExtractedFiles exercises the guest-path mapping directly by
// laying out an already-"extracted" package tree on disk (no archive needed, so
// it runs on every OS). It asserts the four behaviors ExtractedFiles owns:
// top-level files map to their basename, sysroot files map to their
// sysroot-relative path, store metadata is skipped, and build-only subtrees are
// pruned.
func TestOpsStore_ExtractedFiles(t *testing.T) {
	t.Parallel()

	store, err := NewOpsStore(t.TempDir())
	require.NoError(t, err)

	const ns, name, version = "eyberg", "python3", "3.12.3"
	dir := store.PackageDir(ns, name, version)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	// Top-level program -> guest path is its basename.
	writePkgFile(t, dir, "python3", "\x7fELFbinary")
	// Store metadata and archive artifacts -> skipped, never emitted.
	writePkgFile(t, dir, "package.manifest", `{"Program":"python3"}`)
	writePkgFile(t, dir, "manifest.json", `{}`)
	writePkgFile(t, dir, "x86_64.tar.gz", "gzip-bytes")
	writePkgFile(t, dir, "._resource", "apple double")
	// sysroot files -> guest path is the sysroot-relative path.
	writePkgFile(t, dir, "sysroot/lib/python3.12/os.py", "import sys")
	writePkgFile(t, dir, "sysroot/etc/ssl/certs/ca.crt", "cert")
	// Build-only subtrees under python* -> whole subtree pruned.
	writePkgFile(t, dir, "sysroot/lib/python3.12/test/test_os.py", "assert True")
	writePkgFile(t, dir, "sysroot/lib/python3.12/config-3.12-x86_64/libpython.a", "static lib")
	// Empty sysroot directory -> emitted as an IsDir entry so mkfs creates it.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sysroot", "etc", "empty"), 0o755))

	files, err := store.ExtractedFiles(ns, name, version)
	require.NoError(t, err)

	guest := make(map[string]File, len(files))
	for _, f := range files {
		guest[f.GuestPath] = f
	}

	// Kept, mapped to the right guest path, and each host path really exists.
	for _, want := range []string{"python3", "lib/python3.12/os.py", "etc/ssl/certs/ca.crt"} {
		f, ok := guest[want]
		require.True(t, ok, "expected guest path %q", want)
		require.False(t, f.IsDir, "%q should be a regular file", want)
		_, statErr := os.Stat(f.HostPath)
		require.NoError(t, statErr, "host path for %q should exist", want)
	}

	// Empty sysroot dir is emitted as a directory with no host file.
	emptyDir, ok := guest["etc/empty"]
	require.True(t, ok, "empty sysroot dir should be emitted")
	require.True(t, emptyDir.IsDir)
	require.Empty(t, emptyDir.HostPath, "directory entries carry no host path")

	// Store metadata, archive artifacts, and AppleDouble files are never emitted.
	for _, skipped := range []string{"package.manifest", "manifest.json", "x86_64.tar.gz", "._resource"} {
		_, ok := guest[skipped]
		require.False(t, ok, "%q must not be emitted", skipped)
	}

	// Build-only subtrees are pruned wholesale.
	for _, pruned := range []string{
		"lib/python3.12/test/test_os.py",
		"lib/python3.12/config-3.12-x86_64/libpython.a",
	} {
		_, ok := guest[pruned]
		require.False(t, ok, "build-only path %q must be pruned", pruned)
	}
}

// TestOpsStore_ExtractedFiles_RejectsMaliciousRef confirms the coordinate
// validation guards ExtractedFiles just like the other store operations.
func TestOpsStore_ExtractedFiles_RejectsMaliciousRef(t *testing.T) {
	t.Parallel()

	store, err := NewOpsStore(t.TempDir())
	require.NoError(t, err)

	files, err := store.ExtractedFiles("../../etc", "passwd", "1.0")
	require.Error(t, err)
	require.Nil(t, files)
}

// TestOpsStore_Download_Concurrent hammers Download for the same package from
// many goroutines while other goroutines read the store, to exercise the
// RWMutex under the race detector (`go test -race`). The store's contract is
// that concurrent downloads of the same coordinate are safe and idempotent:
// every call returns nil and exactly one archive ends up on disk. Without the
// lock (or with a broken one) -race flags the concurrent map/FS access, or two
// downloaders race to create the archive and the count assertion fails.
func TestOpsStore_Download_Concurrent(t *testing.T) {
	content := []byte("concurrent ops package payload")
	sum := sha256.Sum256(content)
	shaHex := hex.EncodeToString(sum[:])

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(ts.Close)

	origURL := OpsPackageBaseURL
	OpsPackageBaseURL = ts.URL
	t.Cleanup(func() { OpsPackageBaseURL = origURL })

	store, err := NewOpsStore(t.TempDir())
	require.NoError(t, err)

	const workers = 24
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Mix writers and readers on the same coordinate.
			if i%3 == 0 {
				_ = store.IsDownloaded("eyberg", "node", "v16.5.0")
				_, _ = store.List()
				return
			}
			errs <- store.Download("eyberg", "node", "v16.5.0", shaHex)
		}()
	}
	wg.Wait()
	close(errs)

	for e := range errs {
		require.NoError(t, e, "concurrent Download must be safe and idempotent")
	}

	// Exactly one archive on disk despite many concurrent downloaders.
	dir := store.PackageDir("eyberg", "node", "v16.5.0")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	archives := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".gz" {
			archives++
		}
	}
	require.Equal(t, 1, archives, "concurrent downloads must produce exactly one archive")
	require.True(t, store.IsDownloaded("eyberg", "node", "v16.5.0"))
}

// TestDefaultOpsStore points HOME/USERPROFILE at a temp dir so the default
// store is created there rather than polluting the real home directory, then
// checks both the resolved path and that the directory is created.
func TestDefaultOpsStore(t *testing.T) {
	home := t.TempDir()
	// os.UserHomeDir reads USERPROFILE on Windows and HOME elsewhere; set both
	// so the test is portable.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	want := filepath.Join(home, ".jerboa", "packages-ops")
	require.Equal(t, want, opsPackageStoreDir())

	store, err := DefaultOpsStore()
	require.NoError(t, err)
	if ArchSlug() != "amd64" {
		want = filepath.Join(want, ArchSlug())
	}
	require.Equal(t, want, store.root)

	info, err := os.Stat(want)
	require.NoError(t, err, "DefaultOpsStore should create the store directory")
	require.True(t, info.IsDir())
}
