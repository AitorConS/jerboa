//go:build linux

package wslboot

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/apiserver"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

func TestHealthy(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "jerboad.sock")
	mgr := vm.NewQEMUManager("fake-qemu")
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := apiserver.NewServer(mgr, netStore, socketPath, nil, "test", nil)
	require.NoError(t, err)

	ctx := t.Context()
	go func() { _ = srv.Serve(ctx) }()

	require.Eventually(t, func() bool {
		return healthy(ctx, socketPath, "")
	}, 2*time.Second, 20*time.Millisecond)

	// A bogus endpoint is never healthy.
	require.False(t, healthy(ctx, filepath.Join(t.TempDir(), "absent.sock"), ""))
}
