package pkg

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStore_CreateAndList(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkg := Package{
		Name:        "node",
		Version:     "20.11.0",
		Description: "Node.js JavaScript runtime",
		Runtime:     "node",
		SHA256:      "abc123",
		Size:        1024,
	}
	require.NoError(t, store.SaveMeta(pkg))

	pkgs, err := store.List()
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Equal(t, "node", pkgs[0].Name)
	require.Equal(t, "20.11.0", pkgs[0].Version)
}

func TestStore_Remove(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkg := Package{Name: "python", Version: "3.12.0"}
	require.NoError(t, store.SaveMeta(pkg))

	require.NoError(t, store.Remove("python", "3.12.0"))

	pkgs, err := store.List()
	require.NoError(t, err)
	require.Len(t, pkgs, 0)
}

func TestStore_Remove_NotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	require.NoError(t, store.Remove("nonexistent", "1.0.0"))
}

func TestStore_PackageDir(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	result := store.PackageDir("node", "20.11.0")
	require.Equal(t, filepath.Join(dir, "node", "20.11.0"), result)
}

func TestStore_IsDownloaded(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	require.False(t, store.IsDownloaded("node", "20.11.0"))

	pkgDir := store.PackageDir("node", "20.11.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	archivePath := filepath.Join(pkgDir, "files.tar.gz")
	require.NoError(t, os.WriteFile(archivePath, []byte("fake"), 0o644))

	require.True(t, store.IsDownloaded("node", "20.11.0"))
}

func TestIndex_Search(t *testing.T) {
	idx := &Index{
		Packages: map[string][]Package{
			"node":   {{Name: "node", Version: "20.11.0", Description: "Node.js runtime", Runtime: "node"}},
			"python": {{Name: "python", Version: "3.12.0", Description: "Python interpreter", Runtime: "python"}},
			"redis":  {{Name: "redis", Version: "7.2.0", Description: "Redis key-value store", Runtime: "redis"}},
		},
	}

	results := idx.Search("node")
	require.Len(t, results, 1)
	require.Equal(t, "node", results[0].Name)

	results = idx.Search("python")
	require.Len(t, results, 1)
	require.Equal(t, "python", results[0].Name)

	results = idx.Search("key")
	require.Len(t, results, 1)
	require.Equal(t, "redis", results[0].Name)

	results = idx.Search("nonexistent")
	require.Len(t, results, 0)
}

func TestIndex_Latest(t *testing.T) {
	idx := &Index{
		Packages: map[string][]Package{
			"node": {
				{Name: "node", Version: "20.11.0"},
				{Name: "node", Version: "18.19.0"},
			},
		},
	}

	pkg := idx.Latest("node")
	require.NotNil(t, pkg)
	require.Equal(t, "20.11.0", pkg.Version)

	pkg = idx.Latest("nonexistent")
	require.Nil(t, pkg)
}

func TestPackage_JSON(t *testing.T) {
	pkg := Package{
		Name:        "node",
		Version:     "20.11.0",
		Description: "Node.js runtime",
		Runtime:     "node",
		SHA256:      "abc123def456",
		Size:        42_000_000,
		URL:         "https://example.com/node-20.11.0.tar.gz",
	}
	data, err := json.MarshalIndent(pkg, "", "  ")
	require.NoError(t, err)
	require.Contains(t, string(data), `"name": "node"`)

	var decoded Package
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, pkg.Name, decoded.Name)
	require.Equal(t, pkg.Version, decoded.Version)
	require.Equal(t, pkg.SHA256, decoded.SHA256)
}

func TestStore_RemoveAll(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	require.NoError(t, store.SaveMeta(Package{Name: "node", Version: "20.11.0"}))
	require.NoError(t, store.SaveMeta(Package{Name: "node", Version: "18.19.0"}))

	require.NoError(t, store.RemoveAll("node"))

	pkgs, err := store.List()
	require.NoError(t, err)
	require.Len(t, pkgs, 0)
}

func TestStore_RemoveAll_NotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	require.NoError(t, store.RemoveAll("nonexistent"))
}

func TestStore_ExtractAndFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("node", "20.11.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	archivePath := filepath.Join(pkgDir, "files.tar.gz")
	f, err := os.Create(archivePath)
	require.NoError(t, err)

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "bin", Typeflag: tar.TypeDir, Mode: 0o755,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "bin/node", Typeflag: tar.TypeReg, Size: 9, Mode: 0o755,
	}))
	_, err = tw.Write([]byte("fake node"))
	require.NoError(t, err)

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "lib", Typeflag: tar.TypeDir, Mode: 0o755,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "lib/libnode.so", Typeflag: tar.TypeReg, Size: 8, Mode: 0o644,
	}))
	_, err = tw.Write([]byte("fakelib!"))
	require.NoError(t, err)

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	require.NoError(t, f.Close())

	require.False(t, store.IsExtracted("node", "20.11.0"))

	testPkg := Package{Name: "node", Version: "20.11.0"}
	require.NoError(t, store.Extract(testPkg))

	require.True(t, store.IsExtracted("node", "20.11.0"))

	files, err := store.ExtractedFiles("node", "20.11.0")
	require.NoError(t, err)
	require.Len(t, files, 2)

	names := map[string]bool{}
	for _, f := range files {
		names[filepath.Base(f)] = true
	}
	require.True(t, names["node"])
	require.True(t, names["libnode.so"])
}

func TestStore_Extract_Idempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("node", "20.11.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	archivePath := filepath.Join(pkgDir, "files.tar.gz")
	f, err := os.Create(archivePath)
	require.NoError(t, err)

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "hello", Typeflag: tar.TypeReg, Size: 5, Mode: 0o644,
	}))
	_, err = tw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	require.NoError(t, f.Close())

	testPkg := Package{Name: "node", Version: "20.11.0"}
	require.NoError(t, store.Extract(testPkg))
	require.NoError(t, store.Extract(testPkg))
}

func TestStore_Download_VerifiesSHA256(t *testing.T) {
	content := []byte("hello from the package archive")
	sha := sha256.Sum256(content)
	shaHex := hex.EncodeToString(sha[:])

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkg := Package{
		Name:    "testpkg",
		Version: "1.0.0",
		Size:    int64(len(content)),
		SHA256:  shaHex,
		URL:     ts.URL + "/testpkg-1.0.0.tar.gz",
	}
	require.NoError(t, store.Download(pkg))
	require.True(t, store.IsDownloaded("testpkg", "1.0.0"))
}

func TestStore_Download_SHA256Mismatch(t *testing.T) {
	content := []byte("actual content")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkg := Package{
		Name:    "badpkg",
		Version: "1.0.0",
		Size:    int64(len(content)),
		SHA256:  "0000000000000000000000000000000000000000000000000000000000000000",
		URL:     ts.URL + "/badpkg-1.0.0.tar.gz",
	}
	err = store.Download(pkg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sha256 mismatch")

	archivePath := filepath.Join(dir, "badpkg", "1.0.0", "files.tar.gz")
	_, statErr := os.Stat(archivePath)
	require.True(t, os.IsNotExist(statErr), "archive should be removed on sha256 mismatch")
}

func TestStore_Download_SkipsSHA256WhenEmpty(t *testing.T) {
	content := []byte("content without hash check")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkg := Package{
		Name:    "nohash",
		Version: "1.0.0",
		Size:    int64(len(content)),
		SHA256:  "",
		URL:     ts.URL + "/nohash-1.0.0.tar.gz",
	}
	require.NoError(t, store.Download(pkg))
	require.True(t, store.IsDownloaded("nohash", "1.0.0"))
}

func TestStore_Create(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	binaryPath := filepath.Join(t.TempDir(), "myapp")
	require.NoError(t, os.WriteFile(binaryPath, []byte("fake binary content"), 0o755))

	err = store.Create("myapp", "1.0.0", binaryPath, nil, "My test app", "custom")
	require.NoError(t, err)

	require.True(t, store.IsDownloaded("myapp", "1.0.0"))

	pkgs, err := store.List()
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Equal(t, "myapp", pkgs[0].Name)
	require.Equal(t, "1.0.0", pkgs[0].Version)
	require.NotEmpty(t, pkgs[0].SHA256)
	require.Greater(t, pkgs[0].Size, int64(0))
	require.Equal(t, "My test app", pkgs[0].Description)
	require.Equal(t, "custom", pkgs[0].Runtime)
}

func TestStore_Create_WithLibs(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	binaryPath := filepath.Join(t.TempDir(), "myapp")
	require.NoError(t, os.WriteFile(binaryPath, []byte("binary"), 0o755))

	libPath := filepath.Join(t.TempDir(), "libmyapp.so")
	require.NoError(t, os.WriteFile(libPath, []byte("libcontent"), 0o644))

	err = store.Create("myapp", "2.0.0", binaryPath, []string{libPath}, "", "")
	require.NoError(t, err)

	testPkg := Package{Name: "myapp", Version: "2.0.0"}
	require.NoError(t, store.Extract(testPkg))

	files, err := store.ExtractedFiles("myapp", "2.0.0")
	require.NoError(t, err)
	require.Len(t, files, 2)
}

func TestStore_Create_AlreadyExists(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	binaryPath := filepath.Join(t.TempDir(), "myapp")
	require.NoError(t, os.WriteFile(binaryPath, []byte("binary"), 0o755))

	require.NoError(t, store.Create("myapp", "1.0.0", binaryPath, nil, "", ""))

	err = store.Create("myapp", "1.0.0", binaryPath, nil, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

func TestStore_Create_BinaryNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	err = store.Create("myapp", "1.0.0", "/nonexistent/binary", nil, "", "")
	require.Error(t, err)
}

func TestLdd(t *testing.T) {
	binPath, err := os.Executable()
	require.NoError(t, err)

	libs, err := Ldd(binPath)
	if err != nil {
		t.Skipf("ldd not available or binary is static: %v", err)
	}
	require.NotEmpty(t, libs, "expected at least one shared library dependency")
}

func TestMissingFiles(t *testing.T) {
	binPath, err := os.Executable()
	require.NoError(t, err)

	missing, err := MissingFiles(binPath)
	if err != nil {
		t.Skipf("ldd not available: %v", err)
	}
	t.Logf("Missing libraries for %s: %v", binPath, missing)
}

func TestMissingFiles_StaticBinary(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "staticbin")
	require.NoError(t, os.WriteFile(binPath, []byte("not a real binary"), 0o755))

	_, err := Ldd(binPath)
	require.Error(t, err, "ldd on a fake binary should fail")
}

func TestTrimLddAddress(t *testing.T) {
	cases := []struct {
		input  string
		expect string
	}{
		{"/lib/x86_64-linux-gnu/libc.so.6 (0x00007f1234560000)", "/lib/x86_64-linux-gnu/libc.so.6"},
		{"/lib/x86_64-linux-gnu/libc.so.6", "/lib/x86_64-linux-gnu/libc.so.6"},
		{"/lib/ld-linux-x86-64.so.2", "/lib/ld-linux-x86-64.so.2"},
	}
	for _, tc := range cases {
		result := trimLddAddress(tc.input)
		require.Equal(t, tc.expect, result)
	}
}

func TestStore_Push_NotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	err = store.Push("nonexistent", "1.0.0", "http://localhost:0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "read meta")
}

func TestPushMultipart(t *testing.T) {
	meta := Package{
		Name:    "testpkg",
		Version: "1.0.0",
		SHA256:  "abc123",
		Size:    1024,
	}
	metaJSON, err := json.Marshal(meta)
	require.NoError(t, err)

	body, contentType, err := pushMultipart(metaJSON, []byte("fake archive data"))
	require.NoError(t, err)
	require.NotEmpty(t, contentType)
	require.Contains(t, contentType, "multipart/form-data")
	require.True(t, body.Len() > 0)
}
