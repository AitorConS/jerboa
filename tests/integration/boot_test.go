//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/apiserver"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

const (
	defaultSocket  = "/tmp/unid-integration-test.sock"
	defaultQEMU    = "qemu-system-x86_64"
	defaultTimeout = 2 * time.Minute
)

// TestVMLifecycle tests the full create→start→stop→remove cycle via the API.
// Uses a real QEMU process (requires qemu-system-x86_64 in PATH) but a
// trivial image (empty file) — the VM will crash immediately, which is fine
// since we only assert the state machine transitions.
func TestVMLifecycle(t *testing.T) {
	requireQEMU(t)

	img := makeTrivialImage(t)
	mgr := vm.NewQEMUManager(defaultQEMU)

	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := apiserver.NewServer(mgr, netStore, nil, defaultSocket, nil, "", nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	go func() { _ = srv.Serve(ctx) }()

	client := dialWithRetry(t, defaultSocket)
	defer func() { _ = client.Close() }()

	// Run
	info, err := client.Run(ctx, api.RunParams{
		ImagePath: img,
		Memory:    "128M",
		CPUs:      1,
	})
	require.NoError(t, err)
	require.NotEmpty(t, info.ID)

	// List includes the new VM
	list, err := client.List(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, list)

	// Get returns the VM
	got, err := client.Get(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, info.ID, got.ID)

	// Stop (best-effort — VM may have already exited)
	_ = client.Stop(ctx, info.ID, false)

	// Wait for stopped state
	require.Eventually(t, func() bool {
		g, err := client.Get(ctx, info.ID)
		return err == nil && g.State == "stopped"
	}, 30*time.Second, 100*time.Millisecond, "VM did not reach stopped state")

	// Remove
	require.NoError(t, client.Remove(ctx, info.ID))

	list, err = client.List(ctx)
	require.NoError(t, err)
	require.Empty(t, list)
}

func requireQEMU(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(defaultQEMU); err != nil {
		t.Skipf("%s not found in PATH", defaultQEMU)
	}
}

func makeTrivialImage(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.img")
	require.NoError(t, err)
	// 1 MiB of zeros — QEMU will boot and immediately fail/exit, which is fine.
	require.NoError(t, f.Truncate(1<<20))
	require.NoError(t, f.Close())
	return f.Name()
}

func dialWithRetry(t *testing.T, socketPath string) *api.Client {
	t.Helper()
	var client *api.Client
	require.Eventually(t, func() bool {
		var err error
		client, err = api.Dial(filepath.Clean(socketPath))
		return err == nil
	}, 5*time.Second, 50*time.Millisecond, "daemon did not start")
	return client
}
