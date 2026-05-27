package main

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
	"strconv"
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

func TestPkgFromDockerCmd_NoFile(t *testing.T) {
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "pkg", "from-docker", "testpkg:1.0.0", "node:20")
	require.Contains(t, msg, "--file is required")
}

func TestPkgPushCmd_NoVersion(t *testing.T) {
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "pkg", "push", "node", "http://localhost:5000")
	require.Contains(t, msg, "version is required")
}

func TestPkgPushCmd_NotFound(t *testing.T) {
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "pkg", "push", "node:20", "http://localhost:5000")
	require.Contains(t, msg, "not found locally")
}

func TestPkg_Get_AlreadyDownloaded(t *testing.T) {
	archiveData := createTestPackageArchive(t, map[string]string{
		"app": "binary",
	})

	ts, configure := startPkgServer(t)

	configure(pkg.Index{
		Packages: map[string][]pkg.Package{
			"cachedpkg": {
				{
					Name:    "cachedpkg",
					Version: "1.0.0",
					URL:     ts.URL + "/cachedpkg-1.0.0.tar.gz",
					Size:    int64(len(archiveData)),
				},
			},
		},
	}, map[string][]byte{
		"/cachedpkg-1.0.0.tar.gz": archiveData,
	})

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "pkg", "get", "cachedpkg")
	require.Contains(t, out, "cachedpkg")

	out = execRoot(t, socketPath, storePath, "pkg", "get", "cachedpkg")
	require.Contains(t, out, "already downloaded")
}

func TestPkg_Get_LatestVersion(t *testing.T) {
	archiveData := createTestPackageArchive(t, map[string]string{"app": "latest binary"})

	ts, configure := startPkgServer(t)

	configure(pkg.Index{
		Packages: map[string][]pkg.Package{
			"latestpkg": {
				{Name: "latestpkg", Version: "2.0.0", Description: "Latest", Runtime: "go", Size: int64(len(archiveData)), URL: ts.URL + "/latest-2.tar.gz"},
				{Name: "latestpkg", Version: "1.0.0", Description: "Old", Runtime: "go", Size: int64(len(archiveData)), URL: ts.URL + "/latest-1.tar.gz"},
			},
		},
	}, map[string][]byte{
		"/latest-2.tar.gz": archiveData,
		"/latest-1.tar.gz": archiveData,
	})

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "pkg", "get", "latestpkg")
	require.Contains(t, out, "2.0.0")
}

func TestPkg_Get_VersionNotFound(t *testing.T) {
	_, configure := startPkgServer(t)

	configure(pkg.Index{
		Packages: map[string][]pkg.Package{
			"node": {{Name: "node", Version: "20.0.0"}},
		},
	}, nil)

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "pkg", "get", "node:99.0.0")
	require.Contains(t, msg, "not found")
}

func TestPkg_Remove_SpecificVersion(t *testing.T) {
	archiveData := createTestPackageArchive(t, map[string]string{"f.txt": "data"})

	ts, configure := startPkgServer(t)

	configure(pkg.Index{
		Packages: map[string][]pkg.Package{
			"rmver": {
				{Name: "rmver", Version: "2.0.0", Size: int64(len(archiveData)), URL: ts.URL + "/rmver-2.tar.gz"},
				{Name: "rmver", Version: "1.0.0", Size: int64(len(archiveData)), URL: ts.URL + "/rmver-1.tar.gz"},
			},
		},
	}, map[string][]byte{
		"/rmver-2.tar.gz": archiveData,
		"/rmver-1.tar.gz": archiveData,
	})

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	execRoot(t, socketPath, storePath, "pkg", "get", "rmver:2.0.0")
	execRoot(t, socketPath, storePath, "pkg", "get", "rmver:1.0.0")

	out := execRoot(t, socketPath, storePath, "pkg", "list")
	require.Contains(t, out, "rmver")

	out = execRoot(t, socketPath, storePath, "pkg", "remove", "rmver:1.0.0")
	require.Contains(t, out, "1.0.0")

	out = execRoot(t, socketPath, storePath, "pkg", "list")
	require.Contains(t, out, "2.0.0")
}

func TestPkgCreateCmd_WithLibs(t *testing.T) {
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	binaryPath := filepath.Join(t.TempDir(), "myapp")
	require.NoError(t, os.WriteFile(binaryPath, []byte("binary content"), 0o755))

	libPath := filepath.Join(t.TempDir(), "libmyapp.so")
	require.NoError(t, os.WriteFile(libPath, []byte("lib content"), 0o644))

	out := execRoot(t, socketPath, storePath, "pkg", "create", "libpkg:1.0.0", binaryPath, "--libs", libPath, "--description", "With libs", "--runtime", "go")
	require.Contains(t, out, "libpkg:1.0.0")
	require.Contains(t, out, "created")

	out = execRoot(t, socketPath, storePath, "pkg", "list")
	require.Contains(t, out, "libpkg")
}

func TestPkgCreateCmd_Duplicate(t *testing.T) {
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	binaryPath := filepath.Join(t.TempDir(), "dupapp")
	require.NoError(t, os.WriteFile(binaryPath, []byte("binary"), 0o755))

	execRoot(t, socketPath, storePath, "pkg", "create", "duppkg:1.0.0", binaryPath)

	msg := execRootExpectError(t, socketPath, storePath, "pkg", "create", "duppkg:1.0.0", binaryPath)
	require.Contains(t, msg, "already exists")
}

func TestPkgListCmd_JSON(t *testing.T) {
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })

	store, err := pkg.NewStore(pkgStoreDir)
	require.NoError(t, err)
	require.NoError(t, store.SaveMeta(pkg.Package{Name: "jsonpkg", Version: "3.0.0", Description: "JSON test", Runtime: "python", SHA256: "abc123", Size: 1024}))

	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "pkg", "list", "--output-json")
	require.Contains(t, out, "jsonpkg")
	require.Contains(t, out, "3.0.0")
}

func setupOpsServer(t *testing.T) (*httptest.Server, func(list pkg.OpsPackageList, archives map[string][]byte)) {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	origManifestURL := pkg.OpsPackageManifestURL
	pkg.OpsPackageManifestURL = ts.URL + "/manifest.json"
	t.Cleanup(func() { pkg.OpsPackageManifestURL = origManifestURL })

	origBaseURL := pkg.OpsPackageBaseURL
	pkg.OpsPackageBaseURL = ts.URL
	t.Cleanup(func() { pkg.OpsPackageBaseURL = origBaseURL })

	configure := func(list pkg.OpsPackageList, archives map[string][]byte) {
		idxData, err := json.Marshal(list)
		require.NoError(t, err)

		mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(idxData)))
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

func withOpsStoreDir(t *testing.T) {
	t.Helper()
	origDir := opsPkgStoreDir
	opsPkgStoreDir = t.TempDir()
	t.Cleanup(func() { opsPkgStoreDir = origDir })
}

func createOpsTestArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	pr, pw := io.Pipe()
	gw := gzip.NewWriter(pw)
	tw := tar.NewWriter(gw)

	go func() {
		for name, content := range files {
			mode := int64(0o644)
			if name == "node" {
				mode = 0o755
			}
			hdr := &tar.Header{
				Name: name,
				Mode: mode,
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

func TestPkgListOps_Empty(t *testing.T) {
	withOpsStoreDir(t)
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "pkg", "list", "--source", "ops")
	require.Contains(t, out, "No ops packages installed")
}

func TestPkgGetOps_AndList(t *testing.T) {
	archiveData := createOpsTestArchive(t, map[string]string{
		"node": "fake elf binary",
	})

	ts, configure := setupOpsServer(t)
	_ = ts

	h := sha256.Sum256(archiveData)
	sha := hex.EncodeToString(h[:])

	configure(pkg.OpsPackageList{
		Version: 1,
		Packages: []pkg.OpsPackage{
			{Name: "node", Version: "v16.5.0", Namespace: "eyberg", Language: "node", SHA256: sha},
		},
	}, map[string][]byte{
		"/eyberg/node/v16.5.0.tar.gz": archiveData,
	})

	withOpsStoreDir(t)
	_, socketPath := startDaemon(t)
	vmStorePath := t.TempDir()

	out := execRoot(t, socketPath, vmStorePath, "pkg", "get", "eyberg/node:v16.5.0", "--source", "ops")
	require.Contains(t, out, "node")
	require.Contains(t, out, "installed")

	out = execRoot(t, socketPath, vmStorePath, "pkg", "list", "--source", "ops")
	require.Contains(t, out, "eyberg")
	require.Contains(t, out, "node")
}

func TestPkgSearchOps(t *testing.T) {
	_, configure := setupOpsServer(t)

	configure(pkg.OpsPackageList{
		Version: 1,
		Packages: []pkg.OpsPackage{
			{Name: "node", Version: "v16.5.0", Namespace: "eyberg", Language: "node"},
			{Name: "python", Version: "3.12", Namespace: "eyberg", Language: "python"},
		},
	}, nil)

	withOpsStoreDir(t)
	_, socketPath := startDaemon(t)
	vmStorePath := t.TempDir()

	out := execRoot(t, socketPath, vmStorePath, "pkg", "search", "node", "--source", "ops")
	require.Contains(t, out, "node")
}

func TestPkgSearchOps_NoResults(t *testing.T) {
	_, configure := setupOpsServer(t)

	configure(pkg.OpsPackageList{
		Version:  1,
		Packages: []pkg.OpsPackage{},
	}, nil)

	withOpsStoreDir(t)
	_, socketPath := startDaemon(t)
	vmStorePath := t.TempDir()

	out := execRoot(t, socketPath, vmStorePath, "pkg", "search", "nonexistent", "--source", "ops")
	require.Contains(t, out, "No ops packages found")
}

func TestPkgRemoveOps(t *testing.T) {
	archiveData := createOpsTestArchive(t, map[string]string{
		"node": "fake elf binary",
	})

	ts, configure := setupOpsServer(t)
	_ = ts

	h := sha256.Sum256(archiveData)
	sha := hex.EncodeToString(h[:])

	configure(pkg.OpsPackageList{
		Version: 1,
		Packages: []pkg.OpsPackage{
			{Name: "node", Version: "v16.5.0", Namespace: "eyberg", Language: "node", SHA256: sha},
		},
	}, map[string][]byte{
		"/eyberg/node/v16.5.0.tar.gz": archiveData,
	})

	withOpsStoreDir(t)
	_, socketPath := startDaemon(t)
	vmStorePath := t.TempDir()

	execRoot(t, socketPath, vmStorePath, "pkg", "get", "eyberg/node:v16.5.0", "--source", "ops")

	out := execRoot(t, socketPath, vmStorePath, "pkg", "remove", "eyberg/node:v16.5.0", "--source", "ops")
	require.Contains(t, out, "Removed")
}

func TestPkgGetOps_AlreadyDownloaded(t *testing.T) {
	archiveData := createOpsTestArchive(t, map[string]string{
		"node": "fake elf binary",
	})

	ts, configure := setupOpsServer(t)
	_ = ts

	h := sha256.Sum256(archiveData)
	sha := hex.EncodeToString(h[:])

	configure(pkg.OpsPackageList{
		Version: 1,
		Packages: []pkg.OpsPackage{
			{Name: "node", Version: "v16.5.0", Namespace: "eyberg", Language: "node", SHA256: sha},
		},
	}, map[string][]byte{
		"/eyberg/node/v16.5.0.tar.gz": archiveData,
	})

	withOpsStoreDir(t)
	_, socketPath := startDaemon(t)
	vmStorePath := t.TempDir()

	execRoot(t, socketPath, vmStorePath, "pkg", "get", "eyberg/node:v16.5.0", "--source", "ops")

	out := execRoot(t, socketPath, vmStorePath, "pkg", "get", "eyberg/node:v16.5.0", "--source", "ops")
	require.Contains(t, out, "already downloaded")
}

func TestPkgGetOps_NotFound(t *testing.T) {
	_, configure := setupOpsServer(t)

	configure(pkg.OpsPackageList{
		Version:  1,
		Packages: []pkg.OpsPackage{},
	}, nil)

	withOpsStoreDir(t)
	_, socketPath := startDaemon(t)
	vmStorePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, vmStorePath, "pkg", "get", "eyberg/nonexistent:v1", "--source", "ops")
	require.Contains(t, msg, "not found")
}
