package registry_test

import (
	"path/filepath"
	"testing"

	"github.com/AitorConS/unikernel-engine/internal/ociregistry"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/stretchr/testify/require"
)

func TestOCIStore_SaveGetDelete(t *testing.T) {
	t.Parallel()

	store, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)

	m := ociregistry.Manifest{
		SchemaVersion: ociregistry.OCIManifestSchemaVersion,
		MediaType:     ociregistry.MediaTypeImageManifest,
		Config: ociregistry.Descriptor{
			MediaType: ociregistry.MediaTypeImageConfig,
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Size:      1,
		},
		Layers: []ociregistry.Descriptor{{
			MediaType: ociregistry.MediaTypeImageLayerTarGzip,
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Size:      2,
		}},
	}
	digest := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	require.NoError(t, store.Save("app", "latest", digest, m))

	got, gotDigest, err := store.Get("app", "latest")
	require.NoError(t, err)
	require.Equal(t, digest, gotDigest)
	require.Equal(t, m.Config.Digest, got.Config.Digest)

	require.NoError(t, store.Delete("app", "latest"))
	_, _, err = store.Get("app", "latest")
	require.Error(t, err)
}

func TestOCIStore_PersistsAcrossInstances(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "oci")
	store1, err := registry.NewOCIStore(root)
	require.NoError(t, err)

	m := ociregistry.Manifest{
		SchemaVersion: ociregistry.OCIManifestSchemaVersion,
		MediaType:     ociregistry.MediaTypeImageManifest,
		Config: ociregistry.Descriptor{
			MediaType: ociregistry.MediaTypeImageConfig,
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Size:      1,
		},
		Layers: []ociregistry.Descriptor{{
			MediaType: ociregistry.MediaTypeImageLayerTarGzip,
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Size:      2,
		}},
	}
	digest := "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	require.NoError(t, store1.Save("persist", "v1", digest, m))

	store2, err := registry.NewOCIStore(root)
	require.NoError(t, err)
	got, gotDigest, err := store2.Get("persist", "v1")
	require.NoError(t, err)
	require.Equal(t, digest, gotDigest)
	require.Equal(t, m.Config.Digest, got.Config.Digest)

	repos, err := store2.Repositories()
	require.NoError(t, err)
	require.Contains(t, repos, "persist")
}
