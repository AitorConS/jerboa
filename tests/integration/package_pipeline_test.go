package integration

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
	"testing"

	"github.com/AitorConS/unikernel-engine/internal/image"
	pkg "github.com/AitorConS/unikernel-engine/internal/package"
	"github.com/stretchr/testify/require"
)

func createTarGz(t *testing.T, files map[string]string) []byte {
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

func sha256Hex(t *testing.T, data []byte) string {
	t.Helper()
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestPackageBuildPipeline(t *testing.T) {
	archiveData := createTarGz(t, map[string]string{
		"bin/app":    "app binary",
		"lib/lib.so": "shared object",
	})

	shaHex := sha256Hex(t, archiveData)

	testPkg := pkg.Package{
		Name:    "myruntime",
		Version: "1.0.0",
		Size:    int64(len(archiveData)),
		SHA256:  shaHex,
		URL:     "/myruntime-1.0.0.tar.gz",
	}

	idx := pkg.Index{
		Packages: map[string][]pkg.Package{
			"myruntime": {testPkg},
		},
	}
	idxData, err := json.Marshal(idx)
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/packages.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(idxData)
	})
	mux.HandleFunc("/myruntime-1.0.0.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(archiveData)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	origURL := pkg.IndexURL
	pkg.IndexURL = ts.URL + "/packages.json"
	t.Cleanup(func() { pkg.IndexURL = origURL })

	storeDir := t.TempDir()
	store, err := pkg.NewStore(storeDir)
	require.NoError(t, err)

	testPkg.URL = ts.URL + "/myruntime-1.0.0.tar.gz"
	require.NoError(t, store.Download(testPkg))
	require.True(t, store.IsDownloaded("myruntime", "1.0.0"))

	require.NoError(t, store.Extract(testPkg))
	require.True(t, store.IsExtracted("myruntime", "1.0.0"))

	files, err := store.ExtractedFiles("myruntime", "1.0.0")
	require.NoError(t, err)
	require.Len(t, files, 2)

	binPath := filepath.Join(t.TempDir(), "testapp")
	require.NoError(t, os.WriteFile(binPath, []byte("\x7fELFfake"), 0o755))

	var pkgFiles []pkg.File
	for _, f := range files {
		pkgFiles = append(pkgFiles, pkg.File{HostPath: f, GuestPath: filepath.Base(f)})
	}

	got := image.BuildManifest(binPath, pkgFiles, "")

	require.Contains(t, got, "app:(contents:(host:")
	require.Contains(t, got, "lib.so:(contents:(host:")
	require.Contains(t, got, "program:/program")
}
