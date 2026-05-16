package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	pkg "github.com/AitorConS/unikernel-engine/internal/package"
	"github.com/stretchr/testify/require"
)

func createTestPackageArchive(t *testing.T, files map[string]string) []byte {
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

func startPkgServer(t *testing.T) (*httptest.Server, func(idx pkg.Index, archives map[string][]byte)) {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	origURL := pkg.IndexURL
	pkg.IndexURL = ts.URL + "/packages.json"
	t.Cleanup(func() { pkg.IndexURL = origURL })

	configure := func(idx pkg.Index, archives map[string][]byte) {
		idxData, err := json.Marshal(idx)
		require.NoError(t, err)

		mux.HandleFunc("/packages.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(idxData)
		})
		for path, data := range archives {
			mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/gzip")
				_, _ = w.Write(data)
			})
		}
	}

	return ts, configure
}

func TestPkg_Search(t *testing.T) {
	_, configure := startPkgServer(t)

	configure(pkg.Index{
		Packages: map[string][]pkg.Package{
			"hellopkg": {
				{Name: "hellopkg", Version: "1.0.0", Description: "A test package", Runtime: "test"},
			},
		},
	}, nil)

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "pkg", "search", "hello")
	require.Contains(t, out, "hellopkg")
}

func TestPkg_Search_JSON(t *testing.T) {
	_, configure := startPkgServer(t)

	configure(pkg.Index{
		Packages: map[string][]pkg.Package{
			"hellopkg": {
				{Name: "hellopkg", Version: "1.0.0", Description: "A test package", Runtime: "test"},
			},
		},
	}, nil)

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "--output", "json", "pkg", "search", "hello")
	require.Contains(t, out, "hellopkg")
}

func TestPkg_Search_NoResults(t *testing.T) {
	_, configure := startPkgServer(t)

	configure(pkg.Index{Packages: map[string][]pkg.Package{}}, nil)

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "pkg", "search", "nonexistent")
	require.Contains(t, out, "No packages found")
}

func TestPkg_GetAndListAndRemove(t *testing.T) {
	archiveData := createTestPackageArchive(t, map[string]string{
		"bin/app":    "binary content here",
		"lib/lib.so": "shared lib content",
	})

	ts, configure := startPkgServer(t)

	configure(pkg.Index{
		Packages: map[string][]pkg.Package{
			"hellopkg": {
				{
					Name:        "hellopkg",
					Version:     "1.0.0",
					Description: "A test package",
					Runtime:     "test",
					Size:        int64(len(archiveData)),
					URL:         ts.URL + "/hellopkg-1.0.0.tar.gz",
				},
			},
		},
	}, map[string][]byte{
		"/hellopkg-1.0.0.tar.gz": archiveData,
	})

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "pkg", "get", "hellopkg")
	require.Contains(t, out, "hellopkg")

	out = execRoot(t, socketPath, storePath, "pkg", "list")
	require.Contains(t, out, "hellopkg")

	out = execRoot(t, socketPath, storePath, "pkg", "remove", "hellopkg")
	require.Contains(t, out, "Removed all versions")
}

func TestPkg_Get_SpecificVersion(t *testing.T) {
	archiveData := createTestPackageArchive(t, map[string]string{
		"README.md": "hello world",
	})

	ts, configure := startPkgServer(t)

	configure(pkg.Index{
		Packages: map[string][]pkg.Package{
			"verpkg": {
				{
					Name:        "verpkg",
					Version:     "2.0.0",
					Description: "Version two",
					Runtime:     "test",
					Size:        int64(len(archiveData)),
					URL:         ts.URL + "/v2.tar.gz",
				},
				{
					Name:        "verpkg",
					Version:     "1.0.0",
					Description: "Version one",
					Runtime:     "test",
					Size:        int64(len(archiveData)),
					URL:         ts.URL + "/v1.tar.gz",
				},
			},
		},
	}, map[string][]byte{
		"/v2.tar.gz": archiveData,
		"/v1.tar.gz": archiveData,
	})

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "pkg", "get", "verpkg:1.0.0")
	require.Contains(t, out, "1.0.0")

	out = execRoot(t, socketPath, storePath, "pkg", "remove", "verpkg:1.0.0")
	require.Contains(t, out, "1.0.0")
}

func TestPkg_RemoveAllVersions(t *testing.T) {
	archiveData := createTestPackageArchive(t, map[string]string{"f.txt": "data"})

	ts, configure := startPkgServer(t)

	configure(pkg.Index{
		Packages: map[string][]pkg.Package{
			"multiver": {
				{
					Name: "multiver", Version: "3.0.0", Runtime: "test",
					Size: int64(len(archiveData)), URL: ts.URL + "/3.0.0.tar.gz",
				},
				{
					Name: "multiver", Version: "2.0.0", Runtime: "test",
					Size: int64(len(archiveData)), URL: ts.URL + "/2.0.0.tar.gz",
				},
			},
		},
	}, map[string][]byte{
		"/3.0.0.tar.gz": archiveData,
		"/2.0.0.tar.gz": archiveData,
	})

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	execRoot(t, socketPath, storePath, "pkg", "get", "multiver:3.0.0")
	execRoot(t, socketPath, storePath, "pkg", "get", "multiver:2.0.0")

	out := execRoot(t, socketPath, storePath, "pkg", "list")
	require.Contains(t, out, "multiver")

	out = execRoot(t, socketPath, storePath, "pkg", "remove", "multiver")
	require.Contains(t, out, "Removed all versions")
}

func TestPkg_Get_NotFound(t *testing.T) {
	_, configure := startPkgServer(t)

	configure(pkg.Index{Packages: map[string][]pkg.Package{}}, nil)

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "pkg", "get", "nonexistent")
	require.Contains(t, msg, "not found")
}

func TestPkg_List_Empty(t *testing.T) {
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "pkg", "list")
	require.Contains(t, out, "No packages installed")
}

func TestParsePkgRef(t *testing.T) {
	cases := []struct {
		ref     string
		name    string
		version string
	}{
		{"node", "node", ""},
		{"node:20", "node", "20"},
		{"node:20.11.0", "node", "20.11.0"},
		{"my-pkg:v1", "my-pkg", "v1"},
	}
	for _, tc := range cases {
		name, version := parsePkgRef(tc.ref)
		require.Equal(t, tc.name, name, "parsePkgRef(%q) name", tc.ref)
		require.Equal(t, tc.version, version, "parsePkgRef(%q) version", tc.ref)
	}
}

func TestPkgCreateCmd(t *testing.T) {
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	binaryPath := filepath.Join(t.TempDir(), "myapp")
	require.NoError(t, os.WriteFile(binaryPath, []byte("fake binary"), 0o755))

	out := execRoot(t, socketPath, storePath, "pkg", "create", "myapp:1.0.0", binaryPath, "--description", "My test app")
	require.Contains(t, out, "myapp:1.0.0")
	require.Contains(t, out, "created")

	out = execRoot(t, socketPath, storePath, "pkg", "list")
	require.Contains(t, out, "myapp")
}

func TestPkgCreateCmd_DefaultVersion(t *testing.T) {
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	binaryPath := filepath.Join(t.TempDir(), "myapp2")
	require.NoError(t, os.WriteFile(binaryPath, []byte("binary"), 0o755))

	out := execRoot(t, socketPath, storePath, "pkg", "create", "myapp2", binaryPath)
	require.Contains(t, out, "myapp2:1.0.0")
}

func TestPkgCreateCmd_NotFound(t *testing.T) {
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "pkg", "create", "bad:1.0.0", "/nonexistent/binary")
	require.Contains(t, msg, "binary not found")
}
