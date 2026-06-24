//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/httpclient"
	"github.com/stretchr/testify/require"
)

func TestCliParseSemver(t *testing.T) {
	tests := []struct {
		input string
		want  [3]int
	}{
		{"v0.1.0", [3]int{0, 1, 0}},
		{"0.1.0", [3]int{0, 1, 0}},
		{"v1.2.3", [3]int{1, 2, 3}},
		{"", [3]int{0, 0, 0}},
		{"1", [3]int{1, 0, 0}},
		{"bad", [3]int{0, 0, 0}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := cliParseSemver(tt.input)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestCliSemverGT(t *testing.T) {
	require.True(t, cliSemverGT("v0.2.0", "v0.1.0"))
	require.True(t, cliSemverGT("v1.0.0", "v0.99.99"))
	require.False(t, cliSemverGT("v0.1.0", "v0.1.0"))
	require.False(t, cliSemverGT("v0.1.0", "v0.2.0"))
}

func TestCliIsNewer(t *testing.T) {
	require.True(t, cliIsNewer("v0.1.0", "v0.2.0"))
	require.False(t, cliIsNewer("v0.2.0", "v0.1.0"))
	require.False(t, cliIsNewer("v0.1.0", "v0.1.0"))
}

func TestBinaryName(t *testing.T) {
	require.Equal(t, "jerboa", binaryName("jerboa"))
}

func TestBinaryExt(t *testing.T) {
	require.Empty(t, binaryExt())
}

func TestCleanupBackups(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "jerboa.bak"), []byte("old"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "jerboad.bak"), []byte("old"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "jerboa"), []byte("new"), 0o755))

	cleanupBackups(dir)

	_, err := os.Stat(filepath.Join(dir, "jerboa.bak"))
	require.True(t, os.IsNotExist(err), "jerboa.bak should be deleted")

	_, err = os.Stat(filepath.Join(dir, "jerboad.bak"))
	require.True(t, os.IsNotExist(err), "jerboad.bak should be deleted")

	_, err = os.Stat(filepath.Join(dir, "jerboa"))
	require.NoError(t, err, "jerboa should still exist")
}

func TestInstallBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	dest := filepath.Join(dir, "final-binary")

	require.NoError(t, os.WriteFile(src, []byte("content"), 0o755))
	require.NoError(t, installBinary(src, dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, "content", string(got))
}

func TestDownloadToVerified_ServerError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var buf bytes.Buffer
	err := downloadToVerified(ctx, "http://127.0.0.1:0/nonexistent", &buf)
	require.Error(t, err)
}

type rewriteTransport struct {
	baseURL string
	base    http.RoundTripper
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(rt.baseURL, "http://")
	clone.Host = clone.URL.Host
	return rt.base.RoundTrip(clone)
}

func TestUpgradeListAndCheckCommands(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases") {
			_, _ = io.WriteString(w, `[
				{"tag_name":"v0.3.0"},
				{"tag_name":"v0.2.1"},
				{"tag_name":"kernel-v0.1.0"}
			]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	orig := httpclient.Default
	httpclient.Default = &http.Client{Timeout: 3 * time.Second, Transport: rewriteTransport{baseURL: ts.URL, base: http.DefaultTransport}}
	t.Cleanup(func() { httpclient.Default = orig })

	out := execRoot(t, socketPath, storePath, "upgrade", "list")
	require.Contains(t, out, "v0.3.0")
	require.Contains(t, out, "v0.2.1")

	out = execRoot(t, socketPath, storePath, "upgrade", "check")
	require.Contains(t, out, "Latest:")
}

func TestUpgradeListCommandError(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	orig := httpclient.Default
	httpclient.Default = &http.Client{Timeout: 3 * time.Second, Transport: rewriteTransport{baseURL: ts.URL, base: http.DefaultTransport}}
	t.Cleanup(func() { httpclient.Default = orig })

	msg := execRootExpectError(t, socketPath, storePath, "upgrade", "list")
	require.Contains(t, msg, "upgrade list")
}
