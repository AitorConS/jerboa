package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
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

func createPkgArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	pr, pw := io.Pipe()
	gw := gzip.NewWriter(pw)
	tw := tar.NewWriter(gw)
	go func() {
		for name, content := range files {
			hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
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

func withTempPkgStore(t *testing.T) {
	t.Helper()
	origStoreDir := pkgStoreDir
	pkgStoreDir = t.TempDir()
	t.Cleanup(func() { pkgStoreDir = origStoreDir })
}

func setupResolveServer(t *testing.T, makeIndex func(tsURL string) pkg.Index, archives map[string][]byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.NotFoundHandler())

	tmpMux := http.NewServeMux()
	for path, data := range archives {
		tmpMux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/gzip")
			w.Write(data)
		})
	}
	ts.Config.Handler = tmpMux

	idx := makeIndex(ts.URL)
	idxData, err := json.Marshal(idx)
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/packages.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(idxData)
	})
	for path, data := range archives {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/gzip")
			w.Write(data)
		})
	}
	// Replace handler with the one that has correct URLs
	ts.Config.Handler = mux

	origURL := pkg.IndexURL
	pkg.IndexURL = ts.URL + "/packages.json"
	t.Cleanup(func() { pkg.IndexURL = origURL })

	return ts
}

func TestResolvePackages_DownloadExtractListFiles(t *testing.T) {
	archiveData := createPkgArchive(t, map[string]string{
		"bin/app":    "binary content",
		"lib/lib.so": "shared object content",
	})

	setupResolveServer(t, func(tsURL string) pkg.Index {
		return pkg.Index{
			Packages: map[string][]pkg.Package{
				"myruntime": {
					{
						Name: "myruntime", Version: "1.0.0",
						Size: int64(len(archiveData)), URL: tsURL + "/myruntime-1.0.0.tar.gz",
					},
				},
			},
		}
	}, map[string][]byte{
		"/myruntime-1.0.0.tar.gz": archiveData,
	})

	withTempPkgStore(t)

	files, err := resolvePackages(context.Background(), []string{"myruntime"})
	require.NoError(t, err)
	require.Len(t, files, 2)

	names := map[string]bool{}
	for _, f := range files {
		names[filepath.Base(f.HostPath)] = true
	}
	require.True(t, names["app"])
	require.True(t, names["lib.so"])

	for _, f := range files {
		_, err := os.Stat(f.HostPath)
		require.NoError(t, err, "extracted file should exist: %s", f.HostPath)
	}
}

func TestResolvePackages_SpecificVersion(t *testing.T) {
	archiveV1 := createPkgArchive(t, map[string]string{"app": "v1"})
	archiveV2 := createPkgArchive(t, map[string]string{"app": "v2"})

	setupResolveServer(t, func(tsURL string) pkg.Index {
		return pkg.Index{
			Packages: map[string][]pkg.Package{
				"myrt": {
					{
						Name: "myrt", Version: "2.0.0", Runtime: "test",
						Size: int64(len(archiveV2)), URL: tsURL + "/v2.tar.gz",
					},
					{
						Name: "myrt", Version: "1.0.0", Runtime: "test",
						Size: int64(len(archiveV1)), URL: tsURL + "/v1.tar.gz",
					},
				},
			},
		}
	}, map[string][]byte{
		"/v2.tar.gz": archiveV2,
		"/v1.tar.gz": archiveV1,
	})

	withTempPkgStore(t)

	files, err := resolvePackages(context.Background(), []string{"myrt:1.0.0"})
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "app", filepath.Base(files[0].HostPath))
}

func TestResolvePackages_NotFound(t *testing.T) {
	setupResolveServer(t, func(tsURL string) pkg.Index {
		return pkg.Index{Packages: map[string][]pkg.Package{}}
	}, nil)
	withTempPkgStore(t)

	_, err := resolvePackages(context.Background(), []string{"nonexistent"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestResolvePackages_MultiplePackages(t *testing.T) {
	archiveA := createPkgArchive(t, map[string]string{"a.bin": "aaa"})
	archiveB := createPkgArchive(t, map[string]string{"b.bin": "bbb"})

	setupResolveServer(t, func(tsURL string) pkg.Index {
		return pkg.Index{
			Packages: map[string][]pkg.Package{
				"pkga": {
					{
						Name: "pkga", Version: "1.0.0",
						Size: int64(len(archiveA)), URL: tsURL + "/pkga.tar.gz",
					},
				},
				"pkgb": {
					{
						Name: "pkgb", Version: "2.0.0",
						Size: int64(len(archiveB)), URL: tsURL + "/pkgb.tar.gz",
					},
				},
			},
		}
	}, map[string][]byte{
		"/pkga.tar.gz": archiveA,
		"/pkgb.tar.gz": archiveB,
	})

	withTempPkgStore(t)

	files, err := resolvePackages(context.Background(), []string{"pkga", "pkgb"})
	require.NoError(t, err)
	require.Len(t, files, 2)

	names := map[string]bool{}
	for _, f := range files {
		names[filepath.Base(f.HostPath)] = true
	}
	require.True(t, names["a.bin"])
	require.True(t, names["b.bin"])
}

func createOpsResolveArchive(t *testing.T, files map[string]string) []byte {
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
			hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}
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

func withTempOpsStore(t *testing.T) {
	t.Helper()
	origDir := opsPkgStoreDir
	opsPkgStoreDir = t.TempDir()
	t.Cleanup(func() { opsPkgStoreDir = origDir })
}

func setupOpsResolveServer(t *testing.T, list pkg.OpsPackageList, archives map[string][]byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.NotFoundHandler())

	origManifestURL := pkg.OpsPackageManifestURL
	pkg.OpsPackageManifestURL = ts.URL + "/manifest.json"
	t.Cleanup(func() { pkg.OpsPackageManifestURL = origManifestURL })

	origBaseURL := pkg.OpsPackageBaseURL
	pkg.OpsPackageBaseURL = ts.URL
	t.Cleanup(func() { pkg.OpsPackageBaseURL = origBaseURL })

	idxData, err := json.Marshal(list)
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(idxData)))
		w.Write(idxData)
	})
	for path, data := range archives {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/gzip")
			w.Write(data)
		})
	}
	ts.Config.Handler = mux

	return ts
}

func TestResolveOpsPackages_DownloadExtractList(t *testing.T) {
	archiveData := createOpsResolveArchive(t, map[string]string{
		"sysroot/lib/x86_64-linux-gnu/libc.so": "libc content",
		"node": "fake elf binary",
	})

	h := sha256.Sum256(archiveData)
	sha := hex.EncodeToString(h[:])

	setupOpsResolveServer(t, pkg.OpsPackageList{
		Version: 1,
		Packages: []pkg.OpsPackage{
			{Name: "node", Version: "v16.5.0", Namespace: "eyberg", Language: "node", SHA256: sha},
		},
	}, map[string][]byte{
		"/eyberg/node/v16.5.0.tar.gz": archiveData,
	})

	withTempOpsStore(t)

	files, err := resolveOpsPackages(context.Background(), []string{"eyberg/node:v16.5.0"})
	require.NoError(t, err)
	require.Len(t, files, 2)

	guestPaths := map[string]bool{}
	for _, f := range files {
		guestPaths[f.GuestPath] = true
		_, statErr := os.Stat(f.HostPath)
		require.NoError(t, statErr, "host file should exist: %s", f.HostPath)
	}
	require.True(t, guestPaths["lib/x86_64-linux-gnu/libc.so"])
}

func TestResolveOpsPackages_NotFound(t *testing.T) {
	setupOpsResolveServer(t, pkg.OpsPackageList{
		Version:  1,
		Packages: []pkg.OpsPackage{},
	}, nil)
	withTempOpsStore(t)

	_, err := resolveOpsPackages(context.Background(), []string{"eyberg/nonexistent:v1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestResolveOpsPackages_AlreadyDownloaded(t *testing.T) {
	archiveData := createOpsResolveArchive(t, map[string]string{
		"node": "fake elf binary",
	})

	h := sha256.Sum256(archiveData)
	sha := hex.EncodeToString(h[:])

	setupOpsResolveServer(t, pkg.OpsPackageList{
		Version: 1,
		Packages: []pkg.OpsPackage{
			{Name: "node", Version: "v16.5.0", Namespace: "eyberg", Language: "node", SHA256: sha},
		},
	}, map[string][]byte{
		"/eyberg/node/v16.5.0.tar.gz": archiveData,
	})

	withTempOpsStore(t)

	files, err := resolveOpsPackages(context.Background(), []string{"eyberg/node:v16.5.0"})
	require.NoError(t, err)
	require.True(t, len(files) >= 1)

	files2, err := resolveOpsPackages(context.Background(), []string{"eyberg/node:v16.5.0"})
	require.NoError(t, err)
	require.Equal(t, len(files), len(files2))
}
