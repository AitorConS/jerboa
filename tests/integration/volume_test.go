//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/network"
	"github.com/AitorConS/unikernel-engine/internal/tools"
	"github.com/AitorConS/unikernel-engine/internal/vm"
	"github.com/AitorConS/unikernel-engine/internal/volume"
	"github.com/stretchr/testify/require"
)

// TestVolumePersistence verifies that data written to a virtio-blk volume
// survives a VM stop+restart cycle.
func TestVolumePersistence(t *testing.T) {
	requireQEMU(t)

	storeDir := t.TempDir()
	imgStore, err := image.NewStore(filepath.Join(storeDir, "images"))
	require.NoError(t, err)

	voltestBin := filepath.Join(t.TempDir(), "voltest")
	voltestSrc := filepath.Join("..", "..", "examples", "voltest", "main.go")
	require.NoError(t, buildLinuxBinary(voltestSrc, voltestBin), "failed to build voltest binary")

	var mkfsRun image.MkfsFunc
	localTools := filepath.Join("..", "..", "kernel", "output", "tools", "bin")
	if tools.Exist(localTools) {
		mkfsRun, err = tools.ResolveMkfs(context.Background(), localTools, "")
		require.NoError(t, err, "failed to resolve local mkfs")
	} else {
		t.Logf("Local kernel tools not found at %s; downloading from release", localTools)
		mkfsRun, err = tools.ResolveMkfs(context.Background(), filepath.Join(storeDir, "tools"), "")
		require.NoError(t, err, "failed to resolve mkfs")
	}

	builder := image.NewBuilder(imgStore)
	_, err = builder.Build(context.Background(), image.BuildConfig{
		Name:       "voltest",
		Tag:        "latest",
		BinaryPath: voltestBin,
		MkfsRun:    mkfsRun,
		Memory:     "256M",
		CPUs:       1,
	})
	require.NoError(t, err, "failed to build voltest image")

	_, diskPath, err := imgStore.Get("voltest:latest")
	require.NoError(t, err)

	volStore, err := volume.NewStore(filepath.Join(storeDir, "volumes"))
	require.NoError(t, err)
	_, err = volStore.Create("testdata", 1<<30)
	require.NoError(t, err)
	vol, err := volStore.Get("testdata")
	require.NoError(t, err)

	mgr := vm.NewQEMUManager(defaultQEMU)
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := api.NewServer(mgr, netStore, nil, defaultSocket, nil, "", nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

	client := dialWithRetry(t, defaultSocket)
	defer func() { _ = client.Close() }()

	info1, err := client.Run(ctx, api.RunParams{
		ImagePath: diskPath,
		Memory:    "256M",
		CPUs:      1,
		PortMaps: []api.PortMapSpec{
			{HostPort: 18080, GuestPort: 8080, Protocol: "tcp"},
		},
		Volumes: []api.VolumeMountSpec{
			{DiskPath: vol.DiskPath, GuestPath: "/data"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, info1.ID)

	if !waitForHTTP(t, ctx, client, info1.ID, "http://127.0.0.1:18080/", 60*time.Second) {
		dumpVMLogs(t, client, info1.ID, "first run")
		t.Fatal("voltest HTTP server did not become ready on first run")
	}

	resp, err := http.Post("http://127.0.0.1:18080/write?msg=hello", "", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.NoError(t, client.Stop(ctx, info1.ID, false))
	require.Eventually(t, func() bool {
		g, err := client.Get(ctx, info1.ID)
		return err == nil && g.State == "stopped"
	}, 30*time.Second, 100*time.Millisecond)
	require.NoError(t, client.Remove(ctx, info1.ID))

	info2, err := client.Run(ctx, api.RunParams{
		ImagePath: diskPath,
		Memory:    "256M",
		CPUs:      1,
		PortMaps: []api.PortMapSpec{
			{HostPort: 18080, GuestPort: 8080, Protocol: "tcp"},
		},
		Volumes: []api.VolumeMountSpec{
			{DiskPath: vol.DiskPath, GuestPath: "/data"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, info2.ID)

	if !waitForHTTP(t, ctx, client, info2.ID, "http://127.0.0.1:18080/", 60*time.Second) {
		dumpVMLogs(t, client, info2.ID, "second run")
		t.Fatal("voltest HTTP server did not become ready on second run")
	}

	resp, err = http.Get("http://127.0.0.1:18080/")
	require.NoError(t, err)
	body := make([]byte, 1024)
	n, _ := resp.Body.Read(body)
	_ = resp.Body.Close()
	require.Contains(t, string(body[:n]), "hello")

	_ = client.Stop(ctx, info2.ID, false)
	require.Eventually(t, func() bool {
		g, err := client.Get(ctx, info2.ID)
		return err == nil && g.State == "stopped"
	}, 30*time.Second, 100*time.Millisecond)
	require.NoError(t, client.Remove(ctx, info2.ID))
}

func waitForHTTP(t *testing.T, ctx context.Context, client *api.Client, vmID, url string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		g, err := client.Get(ctx, vmID)
		if err == nil && g.State == "stopped" {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func dumpVMLogs(t *testing.T, client *api.Client, vmID, label string) {
	t.Helper()
	logs, err := client.Logs(context.Background(), vmID)
	if err != nil {
		t.Logf("[%s] failed to get VM logs: %v", label, err)
		return
	}
	t.Logf("[%s] VM serial console output:\n%s", label, logs.Logs)
}

func buildLinuxBinary(src, dst string) error {
	cmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", dst, src)
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH=amd64",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build linux binary: %w (output: %s)", err, string(out))
	}
	return nil
}
