package registry_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/ociblob"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/stretchr/testify/require"
)

func TestGarbageCollect_NilStores(t *testing.T) {
	_, err := registry.GarbageCollect(nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "GC requires")
}

func TestGarbageCollect_NilBlobStore(t *testing.T) {
	ociStore, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)
	_, err = registry.GarbageCollect(nil, ociStore)
	require.Error(t, err)
	require.Contains(t, err.Error(), "GC requires")
}

func TestGarbageCollect_NilOCIStore(t *testing.T) {
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	_, err = registry.GarbageCollect(blobs, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "GC requires")
}

func TestGarbageCollect_NoUnreferencedBlobs(t *testing.T) {
	store := makeStore(t)
	root := t.TempDir()
	blobs, err := ociblob.NewStore(filepath.Join(root, "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(root, "oci"))
	require.NoError(t, err)

	h := registry.NewServer(store, registry.WithBlobStore(blobs), registry.WithOCIStore(ociStore)).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := registry.NewClient(srv.URL)

	disk := makeDiskFile(t)
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "keptapp",
		Tag:           "latest",
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc",
		DiskSize:      1024,
	}
	require.NoError(t, client.PushOCI(context.Background(), m, disk))

	result, err := registry.GarbageCollect(blobs, ociStore)
	require.NoError(t, err)
	require.Equal(t, 0, result.Removed)
	require.GreaterOrEqual(t, result.Kept, 1)
}

func TestGarbageCollect_EmptyStores(t *testing.T) {
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociStore, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)

	result, err := registry.GarbageCollect(blobs, ociStore)
	require.NoError(t, err)
	require.Equal(t, 0, result.Removed)
	require.Equal(t, 0, result.Kept)
}

func TestGarbageCollect_ReferencedDigestsError(t *testing.T) {
	blobs, err := ociblob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ociDir := filepath.Join(t.TempDir(), "oci")
	ociStore, err := registry.NewOCIStore(ociDir)
	require.NoError(t, err)

	refsPath := filepath.Join(ociDir, "refs.json")
	require.NoError(t, os.WriteFile(refsPath, []byte("not json"), 0o644))

	_, err = registry.GarbageCollect(blobs, ociStore)
	require.Error(t, err)
	require.Contains(t, err.Error(), "GC load references")
}
