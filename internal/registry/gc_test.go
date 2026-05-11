package registry_test

import (
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

func startTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestGarbageCollect_RemovesUnreferencedBlobs(t *testing.T) {
	store := makeStore(t)
	root := t.TempDir()
	blobs, err := ociblob.NewStore(filepath.Join(root, "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(root, "oci"))
	require.NoError(t, err)

	h := registry.NewServer(store, registry.WithBlobStore(blobs), registry.WithOCIStore(ociStore)).Handler()
	srv := startTestServer(t, h)
	client := registry.NewClient(srv.URL)

	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "gcapp",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	refs, err := ociStore.ReferencedDigests()
	require.NoError(t, err)
	require.NotEmpty(t, refs)

	manifest, digest, err := ociStore.Get("gcapp", "latest")
	require.NoError(t, err)
	require.NoError(t, ociStore.Delete("gcapp", "latest"))
	require.NoError(t, ociStore.Delete("gcapp", digest))
	require.NotEmpty(t, manifest.Layers)

	result, err := registry.GarbageCollect(blobs, ociStore)
	require.NoError(t, err)
	require.GreaterOrEqual(t, result.Removed, 1)
	remaining, err := blobs.List()
	require.NoError(t, err)
	require.Empty(t, remaining)
}
