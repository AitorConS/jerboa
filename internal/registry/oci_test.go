package registry_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/ociblob"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/stretchr/testify/require"
)

func startOCIServer(t *testing.T) (*httptest.Server, *image.Store) {
	t.Helper()
	store := makeStore(t)
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	h := registry.NewServer(store, registry.WithBlobStore(blobs)).Handler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, store
}

func TestOCIBaseAndCatalog(t *testing.T) {
	srv, _ := startOCIServer(t)

	baseResp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	t.Cleanup(func() { _ = baseResp.Body.Close() })
	require.Equal(t, http.StatusUnauthorized, baseResp.StatusCode)

	catResp, err := http.Get(srv.URL + "/v2/_catalog")
	require.NoError(t, err)
	t.Cleanup(func() { _ = catResp.Body.Close() })
	require.Equal(t, http.StatusOK, catResp.StatusCode)
}

func TestOCIUploadBlobRoundTrip(t *testing.T) {
	srv, _ := startOCIServer(t)
	client := registry.NewClient(srv.URL)

	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "ociapp",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}

	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	resp, err := http.Get(srv.URL + "/v2/ociapp/manifests/latest")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/vnd.oci.image.manifest.v1+json", resp.Header.Get("Content-Type"))

	localStore := makeStore(t)
	pulled, err := client.PullOCI(context.Background(), "ociapp:latest", localStore)
	require.NoError(t, err)
	require.Equal(t, "ociapp", pulled.Name)
	require.Equal(t, "latest", pulled.Tag)
}

func TestOCICompleteUploadDigestMismatch(t *testing.T) {
	srv, _ := startOCIServer(t)

	startResp, err := http.Post(srv.URL+"/v2/app/blobs/uploads/", "application/octet-stream", nil)
	require.NoError(t, err)
	loc := startResp.Header.Get("Location")
	require.NoError(t, startResp.Body.Close())
	require.Equal(t, http.StatusAccepted, startResp.StatusCode)

	req, err := http.NewRequest(http.MethodPut, srv.URL+loc+"?digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", bytes.NewReader([]byte("abc")))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
