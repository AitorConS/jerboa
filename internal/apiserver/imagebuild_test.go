//go:build linux

package apiserver_test

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/apiserver"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

// fakeMkfs returns a MkfsFunc that writes a dummy disk image at imgPath and
// returns a trivially succeeding command, so Image.Build can be exercised
// without a real mkfs/kernel toolchain.
func fakeMkfs(t *testing.T) image.MkfsFunc {
	t.Helper()
	return func(ctx context.Context, imgPath, _ string, manifest string) *exec.Cmd {
		require.NoError(t, os.WriteFile(imgPath, []byte("fake-disk:"+manifest), 0o600))
		return exec.CommandContext(ctx, "true")
	}
}

// buildContextTar packs the given guest-path→content map into a tar archive.
func buildContextTar(t *testing.T, files map[string][]byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return &buf
}

func startBuildServer(t *testing.T, store *image.Store) *api.Client {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "jerboad.sock")
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(fakeQEMUCmd(false)))
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := apiserver.NewServer(mgr, netStore, nil, socketPath, nil, "", nil)
	require.NoError(t, err)
	srv.SetImageStore(store)
	srv.EnableImageBuild(fakeMkfs(t))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := srv.Serve(ctx); err != nil {
			t.Logf("server stopped: %v", err)
		}
	}()

	var client *api.Client
	require.Eventually(t, func() bool {
		var dialErr error
		client, dialErr = api.Dial(socketPath)
		return dialErr == nil
	}, 2*time.Second, 10*time.Millisecond, "server did not start")

	t.Cleanup(func() {
		_ = client.Close()
		cancel()
	})
	return client
}

func TestImageBuild_RoundTrip(t *testing.T) {
	store, err := image.NewStore(t.TempDir())
	require.NoError(t, err)
	client := startBuildServer(t, store)

	elf := []byte{0x7f, 'E', 'L', 'F', 0, 1, 2, 3}
	ctxTar := buildContextTar(t, map[string][]byte{
		"app":          elf,
		"lib/extra.so": []byte("library-bytes"),
	})

	res, err := client.ImageBuild(context.Background(), api.BuildParams{
		Name:    "demo",
		Tag:     "v1",
		Program: "app",
		Memory:  "256M",
		CPUs:    2,
		Port:    8080,
	}, ctxTar)
	require.NoError(t, err)

	require.Equal(t, "demo", res.Name)
	require.Equal(t, "v1", res.Tag)
	require.NotEmpty(t, res.DiskDigest)
	require.Positive(t, res.DiskSize)

	// The image must be retrievable from the daemon's store.
	m, diskPath, err := store.Get("demo:v1")
	require.NoError(t, err)
	require.Equal(t, res.DiskDigest, m.DiskDigest)
	require.FileExists(t, diskPath)
	require.Equal(t, 2, m.Config.CPUs)
}

func buildImage(t *testing.T, client *api.Client, name, tag string) {
	t.Helper()
	ctxTar := buildContextTar(t, map[string][]byte{"app": {0x7f, 'E', 'L', 'F', 0, 1}})
	_, err := client.ImageBuild(context.Background(), api.BuildParams{
		Name: name, Tag: tag, Program: "app",
	}, ctxTar)
	require.NoError(t, err)
}

func TestImage_ListAndRemove(t *testing.T) {
	store, err := image.NewStore(t.TempDir())
	require.NoError(t, err)
	client := startBuildServer(t, store)

	buildImage(t, client, "alpha", "v1")
	buildImage(t, client, "beta", "v2")

	list, err := client.ImageList(context.Background())
	require.NoError(t, err)
	names := map[string]bool{}
	for _, m := range list {
		names[m.Name+":"+m.Tag] = true
	}
	require.True(t, names["alpha:v1"])
	require.True(t, names["beta:v2"])

	require.NoError(t, client.ImageRemove(context.Background(), "alpha:v1"))

	list, err = client.ImageList(context.Background())
	require.NoError(t, err)
	for _, m := range list {
		require.NotEqual(t, "alpha", m.Name)
	}
}

func TestVMRun_ByRef(t *testing.T) {
	store, err := image.NewStore(t.TempDir())
	require.NoError(t, err)
	client := startBuildServer(t, store)

	buildImage(t, client, "web", "latest")

	info, err := client.Run(context.Background(), api.RunParams{
		Image:  "web:latest",
		Memory: "256M",
		CPUs:   1,
	})
	require.NoError(t, err)
	require.NotEmpty(t, info.ID)
}

func TestVMRun_ByRef_NotFound(t *testing.T) {
	store, err := image.NewStore(t.TempDir())
	require.NoError(t, err)
	client := startBuildServer(t, store)

	_, err = client.Run(context.Background(), api.RunParams{Image: "ghost:latest", Memory: "256M", CPUs: 1})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestImageBuild_MissingProgram(t *testing.T) {
	store, err := image.NewStore(t.TempDir())
	require.NoError(t, err)
	client := startBuildServer(t, store)

	ctxTar := buildContextTar(t, map[string][]byte{"data.txt": []byte("hello")})

	_, err = client.ImageBuild(context.Background(), api.BuildParams{
		Name:    "demo",
		Program: "missing-binary",
	}, ctxTar)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found in build context")
}

func TestImageBuild_Disabled(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "jerboad.sock")
	mgr := vm.NewQEMUManager("fake-qemu")
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := apiserver.NewServer(mgr, netStore, nil, socketPath, nil, "", nil)
	require.NoError(t, err)
	// EnableImageBuild intentionally not called.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

	var client *api.Client
	require.Eventually(t, func() bool {
		var dialErr error
		client, dialErr = api.Dial(socketPath)
		return dialErr == nil
	}, 2*time.Second, 10*time.Millisecond)
	defer func() { _ = client.Close() }()

	ctxTar := buildContextTar(t, map[string][]byte{"app": {0x7f, 'E', 'L', 'F'}})
	_, err = client.ImageBuild(context.Background(), api.BuildParams{Name: "x", Program: "app"}, ctxTar)
	require.Error(t, err)
	require.Contains(t, err.Error(), "image build disabled")
}
