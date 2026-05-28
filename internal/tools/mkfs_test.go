package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

func TestBuildNanosManifest(t *testing.T) {
	manifest := buildNanosManifest("/abs/path/app")
	require.Contains(t, manifest, "program:(contents:(host:/abs/path/app))")
	require.Contains(t, manifest, "program:/program")
	require.Contains(t, manifest, "environment:()")
}

func TestDirectFunc_DefaultManifest(t *testing.T) {
	mkfs := directFunc("/bin/mkfs", "/tmp/boot.img", "/tmp/kernel.img")
	cmd := mkfs(context.Background(), "/tmp/disk.img", "relative/app", "")

	require.Equal(t, "/bin/mkfs", cmd.Path)
	require.Contains(t, cmd.Args, "-b")
	require.Contains(t, cmd.Args, "/tmp/boot.img")
	require.Contains(t, cmd.Args, "-k")
	require.Contains(t, cmd.Args, "/tmp/kernel.img")
	require.Contains(t, cmd.Args, "/tmp/disk.img")

	data, err := io.ReadAll(cmd.Stdin)
	require.NoError(t, err)
	require.Contains(t, string(data), "program:(contents:(host:")
}

func TestDirectFunc_CustomManifest(t *testing.T) {
	mkfs := directFunc("/bin/mkfs", "/tmp/boot.img", "/tmp/kernel.img")
	cmd := mkfs(context.Background(), "/tmp/disk.img", "bin", "custom-manifest")

	data, err := io.ReadAll(cmd.Stdin)
	require.NoError(t, err)
	require.Equal(t, "custom-manifest", string(data))
}

func TestResolveMkfs_UsesExistingToolsDir(t *testing.T) {
	toolsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "mkfs"), []byte("x"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "kernel.img"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "boot.img"), []byte("x"), 0o644))

	if runtime.GOOS == "windows" {
		_, err := ResolveMkfs(context.Background(), toolsDir, "")
		if err != nil {
			t.Skipf("WSL not available on this host: %v", err)
		}
		return
	}

	mkfs, err := ResolveMkfs(context.Background(), toolsDir, "")
	require.NoError(t, err)
	require.NotNil(t, mkfs)

	cmd := mkfs(context.Background(), "/tmp/disk.img", "app", "")
	require.Equal(t, filepath.Join(toolsDir, "mkfs"), cmd.Path)
}

func TestDownloadArtifact_Success(t *testing.T) {
	t.Parallel()

	content := []byte("artifact")
	hasher := sha256.New()
	hasher.Write(content)
	checksum := hex.EncodeToString(hasher.Sum(nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(checksum + "  mkfs"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "nested", "mkfs")
	err := downloadArtifact(context.Background(), srv.URL+"/mkfs", dest)
	require.NoError(t, err)

	data, readErr := os.ReadFile(dest)
	require.NoError(t, readErr)
	require.Equal(t, string(content), string(data))
}

func TestDownloadArtifact_HTTPError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := downloadArtifact(context.Background(), srv.URL+"/missing", filepath.Join(t.TempDir(), "mkfs"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 404")
}

func TestDownloadArtifact_CreateDirError(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	filePath := filepath.Join(base, "not-a-dir")
	require.NoError(t, os.WriteFile(filePath, []byte("x"), 0o644))
	err := downloadArtifact(context.Background(), "https://example.com/mkfs", filepath.Join(filePath, "mkfs"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "create dir")
}

func TestDownloadArtifact_WriteError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("artifact"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	err := downloadArtifact(context.Background(), srv.URL+"/mkfs", dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "create")
	require.Contains(t, err.Error(), "is a directory")
}

func TestDownloadArtifact_RequestBuildError(t *testing.T) {
	t.Parallel()

	err := downloadArtifact(context.Background(), "://bad-url", filepath.Join(t.TempDir(), "mkfs"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "build request")
}

func TestDownloadArtifact_DownloadError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := downloadArtifact(ctx, "https://example.com/mkfs", filepath.Join(t.TempDir(), "mkfs"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "download")
}

func TestDownloadArtifact_DestinationNameInErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bin", "custom-tool")
	err := downloadArtifact(context.Background(), srv.URL+"/x", dest)
	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprintf("download %s failed", filepath.Base(dest)))
}
