//go:build linux

package apiserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

// newBugServer builds a Server wired to a mock VM manager and a fresh network
// store, sufficient to exercise the run/network handlers directly (no socket).
func newBugServer(t *testing.T) (*Server, *vm.MockManager, *network.Store) {
	t.Helper()
	mgr := vm.NewMockManager()
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	return &Server{mgr: mgr, netStore: netStore}, mgr, netStore
}

func runParams(t *testing.T, p api.RunParams) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	return raw
}

// ---------------------------------------------------------------------------
// BUG-004: duplicate VM names were accepted (and broke DNS resolution).
// ---------------------------------------------------------------------------

func TestHandleRun_RejectsDuplicateName(t *testing.T) {
	s, _, _ := newBugServer(t)
	ctx := context.Background()

	_, rpcErr := s.handleRun(ctx, runParams(t, api.RunParams{
		ImagePath: "/tmp/a.img", Memory: "256M", Name: "alpha",
	}))
	require.Nil(t, rpcErr)

	_, rpcErr = s.handleRun(ctx, runParams(t, api.RunParams{
		ImagePath: "/tmp/a.img", Memory: "256M", Name: "alpha",
	}))
	require.NotNil(t, rpcErr, "a second VM with an in-use name must be rejected")
	require.Contains(t, rpcErr.Message, "already in use")
}

func TestHandleRun_AllowsDistinctNames(t *testing.T) {
	s, _, _ := newBugServer(t)
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c"} {
		_, rpcErr := s.handleRun(ctx, runParams(t, api.RunParams{
			ImagePath: "/tmp/a.img", Memory: "256M", Name: name,
		}))
		require.Nil(t, rpcErr, "distinct name %q must be accepted", name)
	}
}

// ---------------------------------------------------------------------------
// BUG-003: a duplicate static --ip was accepted onto a colliding address.
// ---------------------------------------------------------------------------

func TestHandleRun_RejectsDuplicateStaticIP(t *testing.T) {
	s, _, netStore := newBugServer(t)
	ctx := context.Background()
	_, err := netStore.Create("testnet", "10.100.0.0/24", "bridge")
	require.NoError(t, err)

	base := api.RunParams{
		ImagePath: "/tmp/a.img", Memory: "256M",
		NetworkName: "testnet", IPAddress: "10.100.0.50", StaticIP: true,
	}
	p1 := base
	p1.Name = "alpha"
	_, rpcErr := s.handleRun(ctx, runParams(t, p1))
	require.Nil(t, rpcErr)

	p2 := base
	p2.Name = "dupip"
	_, rpcErr = s.handleRun(ctx, runParams(t, p2))
	require.NotNil(t, rpcErr, "a second VM with the same static IP must be rejected")
	require.Contains(t, rpcErr.Message, "reserve ip")
}

func TestHandleRun_RejectsAlreadyAllocatedIP(t *testing.T) {
	// All supplied IP addresses must be free, including requests from old clients.
	s, _, netStore := newBugServer(t)
	ctx := context.Background()
	_, err := netStore.Create("testnet", "10.100.0.0/24", "bridge")
	require.NoError(t, err)
	ip, err := netStore.AllocateIP("testnet")
	require.NoError(t, err)

	_, rpcErr := s.handleRun(ctx, runParams(t, api.RunParams{
		ImagePath: "/tmp/a.img", Memory: "256M",
		NetworkName: "testnet", IPAddress: ip.String(), StaticIP: false, Name: "dyn",
	}))
	require.NotNil(t, rpcErr, "an existing reservation cannot be claimed by a new run")
}

func TestHandleRun_ReleasesReservedIPOnCreateFailure(t *testing.T) {
	// A static IP is reserved before Create; if Create fails the reservation must
	// be returned to the pool so the address is not leaked.
	s, mgr, netStore := newBugServer(t)
	ctx := context.Background()
	_, err := netStore.Create("testnet", "10.100.0.0/24", "bridge")
	require.NoError(t, err)
	mgr.CreateFn = func(context.Context, vm.Config) (*vm.VM, error) {
		return nil, assertErr("create boom")
	}

	_, rpcErr := s.handleRun(ctx, runParams(t, api.RunParams{
		ImagePath: "/tmp/a.img", Memory: "256M",
		NetworkName: "testnet", IPAddress: "10.100.0.50", StaticIP: true, Name: "x",
	}))
	require.NotNil(t, rpcErr)

	// The address is free again: reserving it now succeeds.
	require.NoError(t, netStore.ReserveIP("testnet", "10.100.0.50"))
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// BUG-006: network rm was allowed while VMs still used it (and leaked the bridge).
// ---------------------------------------------------------------------------

func TestHandleNetworkRemove_RefusesWhileInUse(t *testing.T) {
	s, mgr, netStore := newBugServer(t)
	ctx := context.Background()
	_, err := netStore.Create("testnet", "10.100.0.0/24", "bridge")
	require.NoError(t, err)

	v, err := mgr.Create(ctx, vm.Config{ImagePath: "/tmp/a.img", Memory: "256M", NetworkName: "testnet"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx, v.ID)) // running on testnet

	nameParam, _ := json.Marshal(struct {
		Name string `json:"name"`
	}{"testnet"})

	_, rpcErr := s.handleNetworkRemove(nameParam)
	require.NotNil(t, rpcErr, "removing a network with a running VM must be refused")
	require.Contains(t, rpcErr.Message, "in use")

	require.NoError(t, mgr.Stop(ctx, v.ID))
	_, rpcErr = s.handleNetworkRemove(nameParam)
	require.Nil(t, rpcErr, "once no running VM uses it, removal succeeds")
}

// ---------------------------------------------------------------------------
// BUG-002 (read-only variant): an unformatted read-only volume failed cryptically.
// ---------------------------------------------------------------------------

func TestEnsureVolumesFormatted_ReadOnlyUnformatted_ClearError(t *testing.T) {
	s := &Server{}
	dir := t.TempDir()
	raw := filepath.Join(dir, "disk.img")
	require.NoError(t, os.WriteFile(raw, make([]byte, 4096), 0o644)) // zero-filled, not TFS

	rpcErr := s.ensureVolumesFormatted(context.Background(), []api.VolumeMountSpec{
		{DiskPath: raw, GuestPath: "/data", ReadOnly: true, Label: "data"},
	})
	require.NotNil(t, rpcErr, "an empty read-only volume must fail with a clear error, not a cryptic guest crash")
	require.Contains(t, rpcErr.Message, "read-only")
}

func TestEnsureVolumesFormatted_ReadOnlyFormatted_OK(t *testing.T) {
	s := &Server{}
	dir := t.TempDir()
	formatted := filepath.Join(dir, "disk.img")
	// TFS magic ("NVMTFS") marks the disk as already formatted.
	require.NoError(t, os.WriteFile(formatted, append([]byte("NVMTFS"), make([]byte, 4096)...), 0o644))

	rpcErr := s.ensureVolumesFormatted(context.Background(), []api.VolumeMountSpec{
		{DiskPath: formatted, GuestPath: "/data", ReadOnly: true, Label: "data"},
	})
	require.Nil(t, rpcErr, "a pre-formatted read-only volume must be accepted")
}
