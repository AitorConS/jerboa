//go:build linux

package apiserver_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/apiserver"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/AitorConS/jerboa/internal/registry/recipe"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

func startRecipeServer(t *testing.T, store *image.Store, blobs *cas.Store) *api.Client {
	t.Helper()
	dir := t.TempDir()
	boot, kernel := filepath.Join(dir, "boot.img"), filepath.Join(dir, "kernel.img")
	require.NoError(t, os.WriteFile(boot, []byte("boot"), 0o644))
	require.NoError(t, os.WriteFile(kernel, []byte("kernel"), 0o644))
	socketPath := filepath.Join(dir, "jerboad.sock")
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(fakeQEMUCmd(false)))
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := apiserver.NewServer(mgr, netStore, socketPath, nil, "", nil)
	require.NoError(t, err)
	srv.SetImageStore(store)
	srv.EnableImageBuild(fakeMkfs(t))
	if blobs != nil {
		srv.EnableRecipes(blobs, func(string) (*image.Toolset, error) {
			return &image.Toolset{Version: "0.2.3", Boot: boot, Kernel: kernel}, nil
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	var client *api.Client
	require.Eventually(t, func() bool {
		var dialErr error
		client, dialErr = api.DialWithToken(socketPath, "")
		return dialErr == nil
	}, 2*time.Second, 10*time.Millisecond, "server did not start")
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
	})
	return client
}

// A build records the recipe of its disk, with every input in the blob store,
// and Image.Recipe returns it.
func TestImageBuildRecordsRecipe(t *testing.T) {
	store, err := image.NewStore(t.TempDir())
	require.NoError(t, err)
	blobs, err := cas.New(t.TempDir())
	require.NoError(t, err)
	client := startRecipeServer(t, store, blobs)

	ctxTar := buildContextTar(t, map[string][]byte{"app": validBuildELF(), "lib/extra.so": []byte("library-bytes")})
	res, err := client.ImageBuild(context.Background(), api.BuildParams{Name: "demo", Tag: "v1", Program: "app"}, ctxTar)
	require.NoError(t, err)

	raw, err := client.ImageRecipe(context.Background(), "demo:v1")
	require.NoError(t, err)
	r, err := recipe.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, res.DiskDigest, r.DiskDigest)
	require.Equal(t, "v0.2.3", r.Toolset.Version)
	for _, b := range r.Blobs {
		require.True(t, blobs.Has(b.Digest), b.Digest)
	}
	m, _, err := store.Resolve("demo:v1")
	require.NoError(t, err)
	d, err := r.Digest()
	require.NoError(t, err)
	require.Equal(t, d, m.Recipe)
}

func TestImageRecipeDisabled(t *testing.T) {
	store, err := image.NewStore(t.TempDir())
	require.NoError(t, err)
	client := startRecipeServer(t, store, nil)
	buildImage(t, client, "demo", "v1")
	_, err = client.ImageRecipe(context.Background(), "demo:v1")
	require.ErrorContains(t, err, "recipes disabled")
}
