//go:build linux

package apiserver_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/apiserver"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/AitorConS/jerboa/internal/registry/registrytest"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

// contentMkfs writes a "disk" that depends only on the manifest without host
// paths and the bytes of every input, so a pulled recipe reassembles it.
const contentMkfs = `#!/bin/sh
for last; do :; done
manifest=$(cat)
printf '%s' "$manifest" | sed 's/host:"[^"]*"/host:X/g' > "$last"
cat "$2" "$4" >> "$last"
printf '%s' "$manifest" | grep -o 'host:"[^"]*"' | sed 's/^host:"//; s/"$//' | while IFS= read -r p; do cat "$p" >> "$last"; done
`

func TestImagePushAndPullThroughTheDaemon(t *testing.T) {
	dir := t.TempDir()
	mkfs := filepath.Join(dir, "mkfs")
	require.NoError(t, os.WriteFile(mkfs, []byte(contentMkfs), 0o755))
	boot, kernel := filepath.Join(dir, "boot.img"), filepath.Join(dir, "kernel.img")
	require.NoError(t, os.WriteFile(boot, []byte("boot"), 0o644))
	require.NoError(t, os.WriteFile(kernel, []byte("kernel"), 0o644))

	store, err := image.NewStore(filepath.Join(dir, "images"))
	require.NoError(t, err)
	blobs, err := cas.New(filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	socket := filepath.Join(dir, "jerboad.sock")
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(fakeQEMUCmd(false)))
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := apiserver.NewServer(mgr, netStore, socket, nil, "", nil)
	require.NoError(t, err)
	srv.SetImageStore(store)
	srv.EnableImageBuild(func(ctx context.Context, out, _ string, manifest string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, mkfs, "-b", boot, "-k", kernel, out)
		cmd.Stdin = strings.NewReader(manifest)
		return cmd
	})
	srv.EnableRecipes(blobs, func(string) (*image.Toolset, error) {
		return &image.Toolset{Version: "0.2.3", Boot: boot, Kernel: kernel}, nil
	})
	srv.EnableRegistry(func(context.Context, string) (string, error) { return mkfs, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	var c *api.Client
	require.Eventually(t, func() bool {
		var dialErr error
		c, dialErr = api.DialWithToken(socket, "")
		return dialErr == nil
	}, 2*time.Second, 10*time.Millisecond)
	defer func() { _ = c.Close() }()

	reg := registrytest.New()
	defer reg.Close()
	ref := reg.Host() + "/alice/web:v1"

	built, err := c.ImageBuild(ctx, api.BuildParams{Name: "web", Tag: "latest", Program: "app"},
		buildContextTar(t, map[string][]byte{"app": validBuildELF(), "lib/extra.so": []byte("library")}))
	require.NoError(t, err)
	tagged, err := c.ImageTag(ctx, api.ImageTagParams{Source: "web:latest", Name: reg.Host() + "/alice/web", Tag: "v1"})
	require.NoError(t, err)
	require.NotEmpty(t, tagged.Recipe)

	pushed, err := c.ImagePush(ctx, api.ImagePushParams{Reference: ref, Token: "push-token"})
	require.NoError(t, err)
	require.Equal(t, pushed.Blobs, pushed.BlobsUploaded)

	// Forget the image and its blobs, as on another machine, then pull it.
	require.NoError(t, c.ImageRemove(ctx, "web:latest"))
	require.NoError(t, c.ImageRemove(ctx, reg.Host()+"/alice/web:v1"))
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "blobs")))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "blobs"), 0o755))

	pulled, err := c.ImagePull(ctx, api.ImagePullParams{Reference: ref})
	require.NoError(t, err)
	require.Equal(t, built.DiskDigest, pulled.Image.DiskDigest)
	require.Equal(t, pulled.Blobs, pulled.BlobsFetched)
	m, disk, err := store.Get(reg.Host() + "/alice/web:v1")
	require.NoError(t, err)
	require.Equal(t, built.DiskDigest, m.DiskDigest)
	require.FileExists(t, disk)

	_, err = c.ImagePush(ctx, api.ImagePushParams{Reference: reg.Host() + "/alice/missing:v1", Token: "t"})
	require.ErrorContains(t, err, "jerboa tag")
}

// Registry names contain a slash; run must resolve them in the store rather
// than treat them as a disk path.
func TestRunResolvesRegistryNamedImage(t *testing.T) {
	store, err := image.NewStore(t.TempDir())
	require.NoError(t, err)
	client := startBuildServer(t, store)
	buildImage(t, client, "web", "latest")
	_, err = client.ImageTag(context.Background(), api.ImageTagParams{Source: "web:latest", Name: "alice/web", Tag: "v1"})
	require.NoError(t, err)
	info, err := client.Run(context.Background(), api.RunParams{Image: "alice/web:v1", Memory: "256M", CPUs: 1})
	require.NoError(t, err)
	require.NotEmpty(t, info.ID)
}
