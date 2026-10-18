package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/AitorConS/jerboa/internal/release"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDownloadKernelFromManifest(t *testing.T) {
	files := map[string]string{
		"kernel.img":    "KERNEL-IMG",
		"boot.img":      "BOOT-IMG",
		"kernel-fc.img": "FC-IMG",
		"mkfs":          "MKFS-BIN",
		"dump":          "DUMP-BIN",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		if body, ok := files[name]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// Build a kernel component whose files point at the test server with correct
	// hashes (the signature layer is exercised in internal/release).
	k := release.Component{Version: "v0.2.1", Files: map[string]release.Asset{}}
	for key, body := range files {
		sum := sha256.Sum256([]byte(body))
		k.Files[key] = release.Asset{
			URL:    srv.URL + "/" + key,
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(body)),
		}
	}

	cl := &release.Client{HTTP: srv.Client()}
	toolsDir := t.TempDir()
	if runtime.GOOS == "darwin" {
		err := DownloadKernelFromManifest(context.Background(), cl, toolsDir, k)
		require.ErrorContains(t, err, "macOS ARM64")
		_, statErr := os.Stat(filepath.Join(toolsDir, "mkfs"))
		require.True(t, os.IsNotExist(statErr))
		return
	}
	require.NoError(t, DownloadKernelFromManifest(context.Background(), cl, toolsDir, k))

	// Every file lands under its local name, and the version is recorded.
	for _, local := range []string{"kernel.img", "boot.img", fcKernelLocalName, "mkfs"} {
		_, err := os.Stat(filepath.Join(toolsDir, local))
		require.NoError(t, err, "missing %s", local)
	}
	// dump is a debugging tool builds never use; ResolveDump fetches it.
	_, err := os.Stat(filepath.Join(toolsDir, "dump"))
	require.True(t, os.IsNotExist(err), "dump is not part of the toolset download")
	assert.Equal(t, "v0.2.1", LocalVersion(toolsDir))
}

func TestDownloadKernelFromManifestMissingRequired(t *testing.T) {
	// A component missing kernel.img must fail rather than silently install a
	// partial toolset.
	k := release.Component{Version: "v1", Files: map[string]release.Asset{
		"boot.img": {URL: "http://x/boot.img", SHA256: "aa"},
	}}
	err := DownloadKernelFromManifest(context.Background(), &release.Client{}, t.TempDir(), k)
	assert.Error(t, err)
}

// The published kernel.img and kernel-fc.img are the same ELF: download it
// once and install it under both names.
func TestDownloadKernelFromManifestFetchesSharedFileOnce(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS builds its toolset locally")
	}
	bodies := map[string]string{"kernel.img": "SAME-KERNEL", "kernel-fc.img": "SAME-KERNEL", "boot.img": "BOOT", "mkfs": "MKFS", "dump": "DUMP"}
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		mu.Lock()
		hits[name]++
		mu.Unlock()
		_, _ = w.Write([]byte(bodies[name]))
	}))
	defer srv.Close()
	k := release.Component{Version: "v0.2.3", Files: map[string]release.Asset{}}
	for key, body := range bodies {
		sum := sha256.Sum256([]byte(body))
		k.Files[key] = release.Asset{URL: srv.URL + "/" + key, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
	}
	toolsDir := t.TempDir()
	require.NoError(t, DownloadKernelFromManifest(context.Background(), &release.Client{HTTP: srv.Client()}, toolsDir, k))
	require.Equal(t, 1, hits["kernel.img"]+hits["kernel-fc.img"], "the shared kernel is downloaded once")
	require.Zero(t, hits["dump"])
	for _, local := range []string{"kernel.img", fcKernelLocalName} {
		data, err := os.ReadFile(filepath.Join(toolsDir, local))
		require.NoError(t, err)
		require.Equal(t, "SAME-KERNEL", string(data))
	}
}
