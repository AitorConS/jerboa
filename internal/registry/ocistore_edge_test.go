package registry_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/unikernel-engine/internal/ociregistry"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/stretchr/testify/require"
)

func TestOCIStore_Get_RepoNotFound(t *testing.T) {
	store, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)

	_, _, err = store.Get("nonexistent", "latest")
	require.Error(t, err)
	require.Contains(t, err.Error(), "repository not found")
}

func TestOCIStore_Get_ManifestNotFoundInRepo(t *testing.T) {
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
	require.NoError(t, store.Save("myapp", "v1", digest, m))

	_, _, err = store.Get("myapp", "nonexistent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "manifest not found")
}

func TestOCIStore_Delete_RepoNotFound(t *testing.T) {
	store, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)

	err = store.Delete("nonexistent", "latest")
	require.Error(t, err)
	require.Contains(t, err.Error(), "repository not found")
}

func TestOCIStore_Delete_ManifestNotFound(t *testing.T) {
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
	require.NoError(t, store.Save("myapp2", "v1", digest, m))

	err = store.Delete("myapp2", "nonexistent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "manifest not found")
}

func TestOCIStore_Delete_LastRefRemovesRepo(t *testing.T) {
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
	digest := "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	require.NoError(t, store.Save("lastref", "v1", digest, m))

	require.NoError(t, store.Delete("lastref", "v1"))
	require.NoError(t, store.Delete("lastref", digest))

	repos, err := store.Repositories()
	require.NoError(t, err)
	require.NotContains(t, repos, "lastref")
}

func TestOCIStore_ReferencedDigests(t *testing.T) {
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
	digest := "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	require.NoError(t, store.Save("refsapp", "v1", digest, m))

	digests, err := store.ReferencedDigests()
	require.NoError(t, err)
	require.Contains(t, digests, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.Contains(t, digests, "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
}

func TestOCIStore_ReferencedDigests_Empty(t *testing.T) {
	store, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)

	digests, err := store.ReferencedDigests()
	require.NoError(t, err)
	require.Empty(t, digests)
}

func TestOCIStore_ReadRefs_Corrupt(t *testing.T) {
	root := t.TempDir()
	ociDir := filepath.Join(root, "oci2")
	store, err := registry.NewOCIStore(ociDir)
	require.NoError(t, err)

	refsPath := filepath.Join(ociDir, "refs.json")
	require.NoError(t, os.WriteFile(refsPath, []byte("not json"), 0o644))

	_, err = store.Repositories()
	require.Error(t, err)
}

func TestOCIStore_Repositories_Empty(t *testing.T) {
	store, err := registry.NewOCIStore(filepath.Join(t.TempDir(), "oci"))
	require.NoError(t, err)

	repos, err := store.Repositories()
	require.NoError(t, err)
	require.Empty(t, repos)
}

func TestOCIStore_ReadManifest_Missing(t *testing.T) {
	root := t.TempDir()
	store, err := registry.NewOCIStore(filepath.Join(root, "oci"))
	require.NoError(t, err)

	refsPath := filepath.Join(root, "oci", "refs.json")
	refs := map[string]map[string]string{
		"ghost": {
			"latest": "sha256:deaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddead",
		},
	}
	data, err := json.MarshalIndent(refs, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(refsPath, data, 0o644))

	digests, err := store.ReferencedDigests()
	require.NoError(t, err)
	require.Empty(t, digests)
}

func TestOCIStore_ReadManifest_Corrupt(t *testing.T) {
	root := t.TempDir()
	ociDir := filepath.Join(root, "oci5")
	store, err := registry.NewOCIStore(ociDir)
	require.NoError(t, err)

	refsPath := filepath.Join(ociDir, "refs.json")
	manifestsDir := filepath.Join(ociDir, "manifests")
	require.NoError(t, os.MkdirAll(manifestsDir, 0o755))

	refs := map[string]map[string]string{
		"corrupt": {
			"latest": "sha256:babababababababababababababababababababababababababababababababa",
		},
	}
	data, _ := json.MarshalIndent(refs, "", "  ")
	require.NoError(t, os.WriteFile(refsPath, data, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(manifestsDir, "sha256_babababababababababababababababababababababababababababababababa.json"), []byte("not json"), 0o644))

	_, _, err = store.Get("corrupt", "latest")
	require.Error(t, err)
}
