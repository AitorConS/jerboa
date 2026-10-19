package tools

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/AitorConS/jerboa/internal/release"
	"github.com/stretchr/testify/require"
)

// minisignKey signs documents in the minisign format release.Client verifies.
type minisignKey struct {
	id   [8]byte
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newMinisignKey(t *testing.T) *minisignKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	k := &minisignKey{pub: pub, priv: priv}
	_, err = rand.Read(k.id[:])
	require.NoError(t, err)
	return k
}

func (k *minisignKey) public() string {
	return base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), k.id[:]...), k.pub...))
}

func (k *minisignKey) sign(msg []byte) []byte {
	sig := ed25519.Sign(k.priv, msg)
	blob := append(append([]byte("Ed"), k.id[:]...), sig...)
	global := ed25519.Sign(k.priv, append(append([]byte{}, sig...), []byte("t")...))
	return []byte(fmt.Sprintf("untrusted comment: t\n%s\ntrusted comment: t\n%s\n",
		base64.StdEncoding.EncodeToString(blob), base64.StdEncoding.EncodeToString(global)))
}

// toolsetOrigin serves a signed toolset for v0.2.3 with an mkfs for this host
// and counts mkfs downloads.
func toolsetOrigin(t *testing.T, mkfs []byte) (*release.Client, *atomic.Int32) {
	t.Helper()
	key := newMinisignKey(t)
	var downloads atomic.Int32
	var doc []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kernel/v0.2.3/toolset.json":
			_, _ = w.Write(doc)
		case "/kernel/v0.2.3/toolset.json.minisig":
			_, _ = w.Write(key.sign(doc))
		case "/kernel/v0.2.3/mkfs-host":
			downloads.Add(1)
			_, _ = w.Write(mkfs)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(mkfs)
	doc = []byte(fmt.Sprintf(`{"version":"v0.2.3","mkfs":{"%s-%s":{"url":"%s/kernel/v0.2.3/mkfs-host","sha256":"%s","size":%d}}}`,
		runtime.GOOS, runtime.GOARCH, srv.URL, hex.EncodeToString(sum[:]), len(mkfs)))
	cl, err := release.NewClient(srv.URL, key.public())
	require.NoError(t, err)
	return cl, &downloads
}

func TestMkfsForVersionDownloadsOnce(t *testing.T) {
	cl, downloads := toolsetOrigin(t, []byte("#!/bin/sh\nexit 0\n"))
	client := func() (*release.Client, error) { return cl, nil }
	dir := t.TempDir()

	path, err := mkfsForVersion(context.Background(), dir, "0.2.3", client)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "versions", "v0.2.3", "mkfs"), path)
	require.True(t, isExecutableFile(path))

	again, err := mkfsForVersion(context.Background(), dir, "v0.2.3", client)
	require.NoError(t, err)
	require.Equal(t, path, again)
	require.Equal(t, int32(1), downloads.Load())
}

// The installed toolset's own mkfs is used when its version matches.
func TestMkfsForVersionPrefersInstalledToolset(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mkfs"), []byte("x"), 0o755))
	require.NoError(t, SaveLocalVersion(dir, "v0.2.3"))
	unreachable := func() (*release.Client, error) { return nil, fmt.Errorf("network must not be used") }

	path, err := mkfsForVersion(context.Background(), dir, "0.2.3", unreachable)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "mkfs"), path)

	_, err = mkfsForVersion(context.Background(), dir, "0.2.4", unreachable)
	require.ErrorContains(t, err, "network must not be used")
}

func TestMkfsForVersionRejectsBadInput(t *testing.T) {
	cl, _ := toolsetOrigin(t, []byte("mkfs"))
	client := func() (*release.Client, error) { return cl, nil }
	_, err := mkfsForVersion(context.Background(), t.TempDir(), "../x", client)
	require.ErrorContains(t, err, "invalid toolset version")
	_, err = mkfsForVersion(context.Background(), t.TempDir(), "0.9.9", client)
	require.ErrorContains(t, err, "toolset v0.9.9")
}
