package pkg

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseOpsIdentifier(t *testing.T) {
	cases := []struct {
		input     string
		namespace string
		name      string
		version   string
		wantErr   bool
	}{
		{"eyberg/node:v16.5.0", "eyberg", "node", "v16.5.0", false},
		{"eyberg/node", "eyberg", "node", "latest", false},
		{"node", "", "", "", true},
		{"gcr.io/distroless/python3-debian12:debug", "gcr.io/distroless", "python3-debian12", "debug", false},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			id, err := ParseOpsIdentifier(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.namespace, id.Namespace)
			require.Equal(t, tc.name, id.Name)
			require.Equal(t, tc.version, id.Version)
		})
	}
}

func TestOpsPackageIdentifier_String(t *testing.T) {
	require.Equal(t, "eyberg/node:v16.5.0", OpsPackageIdentifier{Namespace: "eyberg", Name: "node", Version: "v16.5.0"}.String())
	require.Equal(t, "eyberg/node", OpsPackageIdentifier{Namespace: "eyberg", Name: "node", Version: "latest"}.String())
	require.Equal(t, "eyberg/node", OpsPackageIdentifier{Namespace: "eyberg", Name: "node", Version: ""}.String())
}

func TestFetchOpsManifest_HTTP(t *testing.T) {
	list := OpsPackageList{
		Version: 1,
		Packages: []OpsPackage{
			{Name: "node", Version: "v16.5.0", Namespace: "eyberg", Language: "node", SHA256: "abc123"},
		},
	}
	data, err := json.Marshal(list)
	require.NoError(t, err)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))
	t.Cleanup(ts.Close)

	origURL := OpsPackageManifestURL
	OpsPackageManifestURL = ts.URL + "/manifest.json"
	t.Cleanup(func() { OpsPackageManifestURL = origURL })

	got, err := FetchOpsManifest()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Packages, 1)
	require.Equal(t, "node", got.Packages[0].Name)
	require.Equal(t, "eyberg", got.Packages[0].Namespace)
}

func TestFetchOpsManifest_HTTPError(t *testing.T) {
	origURL := OpsPackageManifestURL
	OpsPackageManifestURL = "http://127.0.0.1:0/nonexistent"
	t.Cleanup(func() { OpsPackageManifestURL = origURL })

	_, err := FetchOpsManifest()
	require.Error(t, err)
}

func TestOpsPackageList_Search(t *testing.T) {
	list := &OpsPackageList{
		Packages: []OpsPackage{
			{Name: "node", Version: "v16.5.0", Namespace: "eyberg", Language: "node"},
			{Name: "python", Version: "3.12", Namespace: "eyberg", Language: "python"},
			{Name: "redis", Version: "7.2", Namespace: "prologic", Description: "Redis key-value store"},
		},
	}

	results := list.Search("node")
	require.Len(t, results, 1)
	require.Equal(t, "node", results[0].Name)

	results = list.Search("eyberg")
	require.Len(t, results, 2)

	results = list.Search("nonexistent")
	require.Empty(t, results)
}

func TestOpsPackageList_Lookup(t *testing.T) {
	list := &OpsPackageList{
		Packages: []OpsPackage{
			{Name: "node", Version: "v16.5.0", Namespace: "eyberg"},
			{Name: "node", Version: "v18.0.0", Namespace: "eyberg"},
		},
	}

	pkg := list.Lookup("eyberg", "node", "v16.5.0")
	require.NotNil(t, pkg)
	require.Equal(t, "v16.5.0", pkg.Version)

	pkg = list.Lookup("eyberg", "node", "latest")
	require.NotNil(t, pkg)

	pkg = list.Lookup("eyberg", "node", "")
	require.NotNil(t, pkg)

	pkg = list.Lookup("eyberg", "nonexistent", "latest")
	require.Nil(t, pkg)

	pkg = list.Lookup("wrong", "node", "v16.5.0")
	require.Nil(t, pkg)
}

func TestOpsPackageList_Lookup_VersionNormalization(t *testing.T) {
	list := &OpsPackageList{
		Packages: []OpsPackage{
			{Name: "node", Version: "v11.5.0", Namespace: "eyberg"},
			{Name: "python", Version: "3.10.6", Namespace: "eyberg"},
		},
	}

	// Major version prefix ("11" matches "v11.5.0")
	pkg := list.Lookup("eyberg", "node", "11")
	require.NotNil(t, pkg)
	require.Equal(t, "v11.5.0", pkg.Version)

	// "v" prefix stripped on query side ("v11" matches "v11.5.0")
	pkg = list.Lookup("eyberg", "node", "v11")
	require.NotNil(t, pkg)
	require.Equal(t, "v11.5.0", pkg.Version)

	// No "v" on stored version, minor prefix ("3.10" matches "3.10.6")
	pkg = list.Lookup("eyberg", "python", "3.10")
	require.NotNil(t, pkg)
	require.Equal(t, "3.10.6", pkg.Version)

	// Full version without "v" ("11.5.0" matches "v11.5.0")
	pkg = list.Lookup("eyberg", "node", "11.5.0")
	require.NotNil(t, pkg)
	require.Equal(t, "v11.5.0", pkg.Version)

	// No false positives ("12" should not match "v11.5.0")
	pkg = list.Lookup("eyberg", "node", "12")
	require.Nil(t, pkg)
}

func TestOpsVersionMatch(t *testing.T) {
	tests := []struct {
		pkgVer   string
		query    string
		expected bool
	}{
		{"v11.5.0", "v11.5.0", true},
		{"v11.5.0", "11.5.0", true},
		{"v11.5.0", "11", true},
		{"v11.5.0", "v11", true},
		{"3.10.6", "3.10.6", true},
		{"3.10.6", "3.10", true},
		{"3.10.6", "3", true},
		{"v11.5.0", "12", false},
		{"v11.5.0", "v12", false},
		{"3.10.6", "3.11", false},
		{"v11.5.0", "115", false},
	}
	for _, tt := range tests {
		t.Run(tt.pkgVer+"/"+tt.query, func(t *testing.T) {
			require.Equal(t, tt.expected, opsVersionMatch(tt.pkgVer, tt.query))
		})
	}
}

func TestOpsStore_DownloadAndExtract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks and some ops package features are Linux-only")
	}

	content := createOpsPackageArchive(t, map[string]string{
		"package.manifest": `{"Program":"node","Args":["/node"],"Version":"v16.5.0"}`,
		"node":             string([]byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0, 'f', 'a', 'k', 'e'}),
		"sysroot/lib/x86_64-linux-gnu/libnss_dns.so.2": "lib content",
		"sysroot/etc/ssl/certs/ca-certificates.crt":    "certs content",
	})
	sha := sha256.Sum256(content)
	shaHex := hex.EncodeToString(sha[:])

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(content)
	}))
	t.Cleanup(ts.Close)

	origURL := OpsPackageBaseURL
	OpsPackageBaseURL = ts.URL
	t.Cleanup(func() { OpsPackageBaseURL = origURL })

	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	require.False(t, store.IsDownloaded("eyberg", "node", "v16.5.0"))

	err = store.Download("eyberg", "node", "v16.5.0", shaHex)
	require.NoError(t, err)
	require.True(t, store.IsDownloaded("eyberg", "node", "v16.5.0"))

	require.False(t, store.IsExtracted("eyberg", "node", "v16.5.0"))
	require.NoError(t, store.Extract("eyberg", "node", "v16.5.0"))
	require.True(t, store.IsExtracted("eyberg", "node", "v16.5.0"))

	files, err := store.ExtractedFiles("eyberg", "node", "v16.5.0")
	require.NoError(t, err)

	guestPaths := map[string]bool{}
	for _, f := range files {
		guestPaths[f.GuestPath] = true
		_, statErr := os.Stat(f.HostPath)
		require.NoError(t, statErr, "host path should exist: %s", f.HostPath)
	}
	require.True(t, guestPaths["node"], "should have node binary at top level")
	require.True(t, guestPaths["lib/x86_64-linux-gnu/libnss_dns.so.2"], "should have sysroot lib")
	require.True(t, guestPaths["etc/ssl/certs/ca-certificates.crt"], "should have sysroot etc")
}

func TestMaterializeLinks(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "usr", "local", "pgsql", "lib")
	require.NoError(t, os.MkdirAll(libDir, 0o755))

	// The real shared library on disk.
	real := filepath.Join(libDir, "libpq.so.5.11")
	require.NoError(t, os.WriteFile(real, []byte("ELF-bytes"), 0o755))

	// libpq.so.5 -> libpq.so.5.11 (soname link), and libpq.so -> libpq.so.5
	// (a link pointing at another link, to exercise the multi-pass resolution).
	links := []pendingLink{
		{path: filepath.Join(libDir, "libpq.so"), linkname: "libpq.so.5"},
		{path: filepath.Join(libDir, "libpq.so.5"), linkname: "libpq.so.5.11"},
	}

	materializeLinks(dir, links)

	for _, name := range []string{"libpq.so.5", "libpq.so"} {
		got, err := os.ReadFile(filepath.Join(libDir, name))
		require.NoError(t, err, "link %q should have been materialized as a real file", name)
		require.Equal(t, "ELF-bytes", string(got), "materialized %q should copy the target's bytes", name)
	}
}

func TestMaterializeLinks_RejectsTraversalTarget(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	require.NoError(t, os.MkdirAll(libDir, 0o755))

	// A secret outside the package dir that a malicious symlink target points at.
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("top-secret"), 0o600))

	// evil.so -> ../../secret.txt would copy a file from outside the package dir.
	links := []pendingLink{
		{path: filepath.Join(libDir, "evil.so"), linkname: "../../secret.txt"},
	}
	materializeLinks(dir, links)

	_, err := os.Stat(filepath.Join(libDir, "evil.so"))
	require.True(t, os.IsNotExist(err), "link with an escaping target must not be materialized")
}

func TestWithinDir(t *testing.T) {
	dir := filepath.FromSlash("/pkgs/foo")
	require.True(t, withinDir(dir, filepath.Join(dir, "lib", "x")))
	require.True(t, withinDir(dir, dir))
	require.False(t, withinDir(dir, filepath.Join(dir, "..", "bar")))
	require.False(t, withinDir(dir, filepath.FromSlash("/etc/passwd")))
}

func TestOpsStore_Download_SHA256Mismatch(t *testing.T) {
	content := []byte("fake package content")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	t.Cleanup(ts.Close)

	origURL := OpsPackageBaseURL
	OpsPackageBaseURL = ts.URL
	t.Cleanup(func() { OpsPackageBaseURL = origURL })

	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	err = store.Download("eyberg", "badpkg", "1.0.0", "0000000000000000000000000000000000000000000000000000000000000000")
	require.Error(t, err)
	require.Contains(t, err.Error(), "sha256 mismatch")

	require.False(t, store.IsDownloaded("eyberg", "badpkg", "1.0.0"))
}

func TestOpsStore_Download_AlreadyDownloaded(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("eyberg", "node", "v16.5.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "amd64.tar.gz"), []byte("fake"), 0o644))

	err = store.Download("eyberg", "node", "v16.5.0", "")
	require.NoError(t, err)
}

func TestOpsStore_Remove(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("eyberg", "node", "v16.5.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	require.NoError(t, store.Remove("eyberg", "node", "v16.5.0"))

	_, statErr := os.Stat(pkgDir)
	require.True(t, os.IsNotExist(statErr))
}

func TestOpsStore_Remove_NotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	require.NoError(t, store.Remove("eyberg", "nonexistent", "1.0.0"))
}

func TestOpsStore_List(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("eyberg", "node", "v16.5.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	pkgs, err := store.List()
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Equal(t, "node", pkgs[0].Name)
	require.Equal(t, "v16.5.0", pkgs[0].Version)
	require.Equal(t, "eyberg", pkgs[0].Namespace)
}

func TestSplitOpsDirName(t *testing.T) {
	name, ver := splitOpsDirName("node_v16.5.0")
	require.Equal(t, "node", name)
	require.Equal(t, "v16.5.0", ver)

	name, ver = splitOpsDirName("node")
	require.Equal(t, "node", name)
	require.Empty(t, ver)
}

func TestOpsStore_LoadPackageManifest(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("eyberg", "node", "v16.5.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	manifestData := `{"Program":"node","Args":["/node","hi.js"],"Version":"v16.5.0","Env":{"FOO":"bar"}}`
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "package.manifest"), []byte(manifestData), 0o644))

	cfg, err := store.LoadPackageManifest("eyberg", "node", "v16.5.0")
	require.NoError(t, err)
	require.Equal(t, "node", cfg.Program)
	require.Equal(t, "v16.5.0", cfg.Version)
	require.Len(t, cfg.Args, 2)
	require.Equal(t, "bar", cfg.Env["FOO"])
}

func TestOpsStore_FindBinary(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("eyberg", "node", "v16.5.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	elfContent := []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0, 'f', 'a', 'k', 'e'}
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "node"), elfContent, 0o755))

	binPath, err := store.FindBinary("eyberg", "node", "v16.5.0")
	require.NoError(t, err)
	require.Contains(t, binPath, "node")
}

func TestOpsStore_FetchManifestCached(t *testing.T) {
	list := OpsPackageList{
		Version:  1,
		Packages: []OpsPackage{{Name: "node", Version: "v16.5.0", Namespace: "eyberg"}},
	}
	data, err := json.Marshal(list)
	require.NoError(t, err)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))
	t.Cleanup(ts.Close)

	origURL := OpsPackageManifestURL
	OpsPackageManifestURL = ts.URL + "/manifest.json"
	t.Cleanup(func() { OpsPackageManifestURL = origURL })

	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	got, err := store.FetchManifestCached()
	require.NoError(t, err)
	require.Len(t, got.Packages, 1)

	got2, err := store.FetchManifestCached()
	require.NoError(t, err)
	require.Len(t, got2.Packages, 1)
}

func TestOpsStore_IsExtracted_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("eyberg", "node", "v20.0.0")
	require.NoError(t, os.MkdirAll(filepath.Join(pkgDir, "sysroot", "lib"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "package.manifest"),
		[]byte(`{"Program":"node_v20.0.0/node","Version":"20.0.0"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "amd64.tar.gz"), []byte("fake"), 0o644))

	// sysroot/ exists but the program binary ("node") does not — should return false.
	require.False(t, store.IsExtracted("eyberg", "node", "v20.0.0"))

	// After placing the binary it should return true.
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "node"), []byte("elf"), 0o755))
	require.True(t, store.IsExtracted("eyberg", "node", "v20.0.0"))
}

func TestOpsStore_FindBinary_PrefixedProgram(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("eyberg", "node", "v20.0.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "package.manifest"),
		[]byte(`{"Program":"node_v20.0.0/node","Version":"20.0.0"}`), 0o644))
	elfContent := []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0, 'f', 'a', 'k', 'e'}
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "node"), elfContent, 0o755))

	binPath, err := store.FindBinary("eyberg", "node", "v20.0.0")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(filepath.ToSlash(binPath), "/node"))
}

func TestOpsStore_Extract_Idempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewOpsStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("eyberg", "node", "v16.5.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	archiveData := createOpsPackageArchive(t, map[string]string{
		"node": "fake elf",
	})
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "amd64.tar.gz"), archiveData, 0o644))

	require.NoError(t, store.Extract("eyberg", "node", "v16.5.0"))
	require.NoError(t, store.Extract("eyberg", "node", "v16.5.0"))
}

func createOpsPackageArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	pr, pw := io.Pipe()
	gw := gzip.NewWriter(pw)
	tw := tar.NewWriter(gw)

	go func() {
		for name, content := range files {
			hdr := &tar.Header{
				Name: name,
				Mode: 0o644,
				Size: int64(len(content)),
			}
			if name == "node" || !strings.Contains(name, "/") && !strings.Contains(name, ".") {
				hdr.Mode = 0o755
			}
			_ = tw.WriteHeader(hdr)
			_, _ = tw.Write([]byte(content))
		}
		_ = tw.Close()
		_ = gw.Close()
		_ = pw.Close()
	}()

	buf, err := io.ReadAll(pr)
	require.NoError(t, err)
	return buf
}
