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

	err = store.Remove("nonexistent", "1.0.0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found locally")
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

	err = store.RemoveAll("nonexistent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found locally")
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

func TestFetchIndex_HTTP(t *testing.T) {
	idx := Index{
		Packages: map[string][]Package{
			"node": {{Name: "node", Version: "20.11.0", Description: "Node.js runtime", Runtime: "node"}},
		},
	}
	idxData, err := json.Marshal(idx)
	require.NoError(t, err)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(idxData)
	}))
	t.Cleanup(ts.Close)

	origURL := IndexURL
	IndexURL = ts.URL + "/packages.json"
	t.Cleanup(func() { IndexURL = origURL })

	got, err := FetchIndex()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Contains(t, got.Packages, "node")
	require.Equal(t, "20.11.0", got.Packages["node"][0].Version)
}

func TestFetchIndex_HTTPError(t *testing.T) {
	origURL := IndexURL
	IndexURL = "http://127.0.0.1:0/nonexistent"
	t.Cleanup(func() { IndexURL = origURL })

	_, err := FetchIndex()
	require.Error(t, err)
}

func TestFetchIndex_InvalidJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json"))
	}))
	t.Cleanup(ts.Close)

	origURL := IndexURL
	IndexURL = ts.URL + "/packages.json"
	t.Cleanup(func() { IndexURL = origURL })

	_, err := FetchIndex()
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse")
}

func TestFetchIndex_HTTPStatusNotOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)

	origURL := IndexURL
	IndexURL = ts.URL + "/packages.json"
	t.Cleanup(func() { IndexURL = origURL })

	_, err := FetchIndex()
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 404")
}

func TestStore_Download_AlreadyDownloaded(t *testing.T) {
	content := []byte("already here")

	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("cached", "1.0.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "files.tar.gz"), content, 0o644))

	pkg := Package{
		Name:    "cached",
		Version: "1.0.0",
		URL:     "http://invalid.unreachable/test.tar.gz",
	}
	require.NoError(t, store.Download(pkg), "should skip download when file already exists")
}

func TestStore_Download_SizeMismatch(t *testing.T) {
	content := []byte("short")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkg := Package{
		Name:    "bigpkg",
		Version: "1.0.0",
		Size:    999999,
		URL:     ts.URL + "/bigpkg-1.0.0.tar.gz",
	}
	err = store.Download(pkg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "size mismatch")

	archivePath := filepath.Join(dir, "bigpkg", "1.0.0", "files.tar.gz")
	_, statErr := os.Stat(archivePath)
	require.True(t, os.IsNotExist(statErr), "archive should be removed on size mismatch")
}

func TestStore_Download_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkg := Package{
		Name:    "failpkg",
		Version: "1.0.0",
		URL:     ts.URL + "/failpkg-1.0.0.tar.gz",
	}
	err = store.Download(pkg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 500")
}

func TestStore_Extract_ArchiveNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkg := Package{Name: "missing", Version: "1.0.0"}
	err = store.Extract(pkg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "extract open")
}

func TestStore_Extract_InvalidGzip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("badgz", "1.0.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "files.tar.gz"), []byte("not a gzip"), 0o644))

	pkg := Package{Name: "badgz", Version: "1.0.0"}
	err = store.Extract(pkg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "gzip")
}

func TestStore_Extract_DirectoryEntries(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("dirpkg", "1.0.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	archivePath := filepath.Join(pkgDir, "files.tar.gz")
	f, err := os.Create(archivePath)
	require.NoError(t, err)

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "usr", Typeflag: tar.TypeDir, Mode: 0o755,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "usr/local", Typeflag: tar.TypeDir, Mode: 0o755,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "usr/local/bin", Typeflag: tar.TypeDir, Mode: 0o755,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "usr/local/bin/app", Typeflag: tar.TypeReg, Size: 3, Mode: 0o755,
	}))
	_, err = tw.Write([]byte("hi!"))
	require.NoError(t, err)

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	require.NoError(t, f.Close())

	require.NoError(t, store.Extract(Package{Name: "dirpkg", Version: "1.0.0"}))

	files, err := store.ExtractedFiles("dirpkg", "1.0.0")
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Contains(t, files[0], "app")
}

func TestStore_Create_WithDescriptionAndRuntime(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	binaryPath := filepath.Join(t.TempDir(), "myapp")
	require.NoError(t, os.WriteFile(binaryPath, []byte("binary"), 0o755))

	require.NoError(t, store.Create("myapp", "1.0.0", binaryPath, nil, "My description", "go"))

	pkgDir := store.PackageDir("myapp", "1.0.0")
	data, err := os.ReadFile(filepath.Join(pkgDir, "meta.json"))
	require.NoError(t, err)

	var meta Package
	require.NoError(t, json.Unmarshal(data, &meta))
	require.Equal(t, "My description", meta.Description)
	require.Equal(t, "go", meta.Runtime)
	require.NotEmpty(t, meta.SHA256)
	require.Greater(t, meta.Size, int64(0))
	require.Equal(t, "myapp", meta.Name)
	require.Equal(t, "1.0.0", meta.Version)
}

func TestStore_SaveMeta(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkg := Package{
		Name:        "meta-test",
		Version:     "2.0.0",
		Description: "test description",
		Runtime:     "python",
		SHA256:      "deadbeef",
		Size:        12345,
	}
	require.NoError(t, store.SaveMeta(pkg))

	pkgs, err := store.List()
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Equal(t, "meta-test", pkgs[0].Name)
	require.Equal(t, "2.0.0", pkgs[0].Version)
	require.Equal(t, "test description", pkgs[0].Description)
	require.Equal(t, "python", pkgs[0].Runtime)
	require.Equal(t, "deadbeef", pkgs[0].SHA256)
	require.Equal(t, int64(12345), pkgs[0].Size)
}

func TestStore_List_CorruptMetaJSON(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("corrupt", "1.0.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "meta.json"), []byte("}{invalid json"), 0o644))

	pkgs, err := store.List()
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Equal(t, "corrupt", pkgs[0].Name)
	require.Equal(t, "1.0.0", pkgs[0].Version)
	require.Empty(t, pkgs[0].Description, "should fall back to zero-value when meta is corrupt")
}

func TestStore_List_NoMetaJSON(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkgDir := store.PackageDir("nometa", "3.0.0")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))

	pkgs, err := store.List()
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Equal(t, "nometa", pkgs[0].Name)
	require.Equal(t, "3.0.0", pkgs[0].Version)
}

func TestStore_List_Empty(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	pkgs, err := store.List()
	require.NoError(t, err)
	require.Len(t, pkgs, 0)
}

func TestStore_List_MultiplePackagesVersions(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	require.NoError(t, store.SaveMeta(Package{Name: "node", Version: "20.0.0", Description: "Node runtime"}))
	require.NoError(t, store.SaveMeta(Package{Name: "node", Version: "18.0.0", Description: "Node runtime"}))
	require.NoError(t, store.SaveMeta(Package{Name: "python", Version: "3.12.0", Description: "Python runtime"}))

	pkgs, err := store.List()
	require.NoError(t, err)
	require.Len(t, pkgs, 3)

	names := map[string]int{}
	for _, p := range pkgs {
		names[p.Name]++
	}
	require.Equal(t, 2, names["node"])
	require.Equal(t, 1, names["python"])
}

func TestStore_Push_Success(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	binaryPath := filepath.Join(t.TempDir(), "pushapp")
	require.NoError(t, os.WriteFile(binaryPath, []byte("binary content"), 0o755))
	require.NoError(t, store.Create("pushapp", "1.0.0", binaryPath, nil, "push test", "go"))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Contains(t, r.Header.Get("Content-Type"), "multipart/form-data")
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(ts.Close)

	require.NoError(t, store.Push("pushapp", "1.0.0", ts.URL))
}

func TestStore_Push_ServerError(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	binaryPath := filepath.Join(t.TempDir(), "failpush")
	require.NoError(t, os.WriteFile(binaryPath, []byte("binary content"), 0o755))
	require.NoError(t, store.Create("failpush", "1.0.0", binaryPath, nil, "", ""))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	t.Cleanup(ts.Close)

	err = store.Push("failpush", "1.0.0", ts.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 500")
}

func TestIndex_Search_Empty(t *testing.T) {
	idx := &Index{Packages: map[string][]Package{}}
	results := idx.Search("anything")
	require.Len(t, results, 0)
}

func TestIndex_Search_CaseInsensitive(t *testing.T) {
	idx := &Index{
		Packages: map[string][]Package{
			"Node": {{Name: "Node", Version: "20.0.0", Description: "JavaScript Runtime"}},
		},
	}
	results := idx.Search("node")
	require.Len(t, results, 1)
	require.Equal(t, "Node", results[0].Name)
}

func TestIndex_Search_MatchesDescription(t *testing.T) {
	idx := &Index{
		Packages: map[string][]Package{
			"redis": {{Name: "redis", Version: "7.2.0", Description: "In-memory key-value store"}},
		},
	}
	results := idx.Search("in-memory")
	require.Len(t, results, 1)
}

func TestIndex_Search_MatchesRuntime(t *testing.T) {
	idx := &Index{
		Packages: map[string][]Package{
			"deno": {{Name: "deno", Version: "1.0.0", Description: "Deno runtime", Runtime: "deno"}},
		},
	}
	results := idx.Search("deno")
	require.Len(t, results, 1)
}

func TestIndex_Latest_Empty(t *testing.T) {
	idx := &Index{Packages: map[string][]Package{}}
	require.Nil(t, idx.Latest("nonexistent"))
}

func TestIndex_Latest_MultipleVersions(t *testing.T) {
	idx := &Index{
		Packages: map[string][]Package{
			"node": {
				{Name: "node", Version: "22.0.0"},
				{Name: "node", Version: "20.0.0"},
				{Name: "node", Version: "18.0.0"},
			},
		},
	}
	require.Equal(t, "22.0.0", idx.Latest("node").Version)
}
