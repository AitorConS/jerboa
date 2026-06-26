//go:build linux

package apiserver_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/apiserver"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

// fakeQEMUCmd returns a vm.CommandFunc suitable for tests.
func fakeQEMUCmd(block bool) vm.CommandFunc {
	return func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if block {
			return exec.Command("sleep", "3600")
		}
		return exec.Command("true")
	}
}

func startTestServer(t *testing.T) (*api.Client, context.CancelFunc) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "jerboad.sock")
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(fakeQEMUCmd(true)))
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := apiserver.NewServer(mgr, netStore, nil, socketPath, nil, "", nil)
	require.NoError(t, err)

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
	return client, cancel
}

func TestServer_Run(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img",
		Memory:    "256M",
		CPUs:      1,
	})
	require.NoError(t, err)
	require.NotEmpty(t, info.ID)
	require.Equal(t, "running", info.State)
}

func TestServer_List(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.Run(context.Background(), api.RunParams{ImagePath: "a.img", Memory: "256M"})
	require.NoError(t, err)
	_, err = client.Run(context.Background(), api.RunParams{ImagePath: "b.img", Memory: "256M"})
	require.NoError(t, err)

	infos, err := client.List(context.Background())
	require.NoError(t, err)
	require.Len(t, infos, 2)
}

func TestServer_Get(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	got, err := client.Get(context.Background(), info.ID)
	require.NoError(t, err)
	require.Equal(t, info.ID, got.ID)
}

func TestServer_Stop(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	require.NoError(t, client.Stop(context.Background(), info.ID, false))

	require.Eventually(t, func() bool {
		got, err := client.Get(context.Background(), info.ID)
		return err == nil && got.State == "stopped"
	}, 5*time.Second, 50*time.Millisecond, "VM did not reach stopped state")
}

func TestServer_Run_WithPortsAndEnv(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img",
		Memory:    "256M",
		CPUs:      1,
		Name:      "myvm",
		// Port publishing requires a TAP network. NetworkName satisfies the
		// guard; with no IPAddress the forwarder no-ops (binds nothing), so the
		// test stays hermetic while still exercising port-config plumbing.
		NetworkName: "testnet",
		Env:         []string{"FOO=bar", "PORT=8080"},
		PortMaps: []api.PortMapSpec{
			{HostPort: 8080, GuestPort: 80, Protocol: "tcp"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "myvm", info.Name)

	detail, err := client.Inspect(context.Background(), info.ID)
	require.NoError(t, err)
	require.Equal(t, "myvm", detail.Name)
	require.Equal(t, []string{"FOO=bar", "PORT=8080"}, detail.Env)
	require.Len(t, detail.Ports, 1)
	require.Equal(t, uint16(8080), detail.Ports[0].HostPort)
	require.Equal(t, uint16(80), detail.Ports[0].GuestPort)
	require.Equal(t, "tcp", detail.Ports[0].Protocol)
}

func TestServer_Run_AutoRemove(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "jerboad.sock")
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(fakeQEMUCmd(false)))
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := apiserver.NewServer(mgr, netStore, nil, socketPath, nil, "", nil)
	require.NoError(t, err)

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

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath:  "test.img",
		Memory:     "256M",
		AutoRemove: true,
	})
	require.NoError(t, err)

	// With auto-remove and a process that exits immediately, the VM should
	// eventually disappear from the list.
	require.Eventually(t, func() bool {
		infos, _ := client.List(context.Background())
		for _, v := range infos {
			if v.ID == info.ID {
				return false
			}
		}
		return true
	}, 5*time.Second, 50*time.Millisecond, "VM was not auto-removed")
}

func TestServer_UnknownMethod(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "jerboad.sock")
	mgr := vm.NewQEMUManager("fake-qemu")
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	srv, err := apiserver.NewServer(mgr, netStore, nil, socketPath, nil, "", nil)
	require.NoError(t, err)

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

	_, err = client.Get(context.Background(), "nonexistent")
	require.Error(t, err)
}

// --- Kill ---

func TestServer_Kill(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	require.NoError(t, client.Kill(context.Background(), info.ID))

	require.Eventually(t, func() bool {
		got, err := client.Get(context.Background(), info.ID)
		return err == nil && got.State == "stopped"
	}, 5*time.Second, 50*time.Millisecond, "VM did not stop after Kill")
}

func TestServer_KillNotFound(t *testing.T) {
	client, _ := startTestServer(t)
	err := client.Kill(context.Background(), "nonexistent")
	require.Error(t, err)
}

// --- Signal ---

func TestServer_Signal(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	// SIGKILL uses proc.Kill() which is cross-platform (TerminateProcess on Windows).
	require.NoError(t, client.Signal(context.Background(), info.ID, "SIGKILL"))
}

func TestServer_SignalInvalid(t *testing.T) {
	client, _ := startTestServer(t)
	err := client.Signal(context.Background(), "any-id", "INVALIDSIG")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown signal")
}

// --- Remove ---

func TestServer_Remove(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	require.NoError(t, client.Stop(context.Background(), info.ID, true))
	require.Eventually(t, func() bool {
		got, err := client.Get(context.Background(), info.ID)
		return err == nil && got.State == "stopped"
	}, 5*time.Second, 50*time.Millisecond)

	require.NoError(t, client.Remove(context.Background(), info.ID))

	_, err = client.Get(context.Background(), info.ID)
	require.Error(t, err)
}

func TestServer_RemoveRunning(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	err = client.Remove(context.Background(), info.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be stopped")
}

func TestServer_RemoveNotFound(t *testing.T) {
	client, _ := startTestServer(t)
	err := client.Remove(context.Background(), "nonexistent")
	require.Error(t, err)
}

// --- Logs ---

func TestServer_Logs(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	resp, err := client.Logs(context.Background(), info.ID)
	require.NoError(t, err)
	require.Equal(t, info.ID, resp.ID)
}

func TestServer_LogsNotFound(t *testing.T) {
	client, _ := startTestServer(t)
	_, err := client.Logs(context.Background(), "nonexistent")
	require.Error(t, err)
}

// --- Inspect ---

func TestServer_Inspect(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img",
		Memory:    "512M",
		CPUs:      2,
		Name:      "inspect-test",
		Env:       []string{"KEY=VAL"},
	})
	require.NoError(t, err)

	detail, err := client.Inspect(context.Background(), info.ID)
	require.NoError(t, err)
	require.Equal(t, info.ID, detail.ID)
	require.Equal(t, "inspect-test", detail.Name)
	require.Equal(t, "512M", detail.Memory)
	require.Equal(t, 2, detail.CPUs)
	require.Equal(t, []string{"KEY=VAL"}, detail.Env)
	require.Equal(t, "running", detail.State)
}

func TestServer_DNSResolveAndList(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.Run(context.Background(), api.RunParams{
		ImagePath:   "test.img",
		Memory:      "256M",
		Name:        "frontend",
		NetworkName: "app",
		IPAddress:   "10.100.1.2",
	})
	require.NoError(t, err)

	rec, err := client.DNSResolve(context.Background(), "frontend", "app")
	require.NoError(t, err)
	require.Equal(t, "10.100.1.2", rec.IP)

	rec, err = client.DNSResolve(context.Background(), "frontend.app", "")
	require.NoError(t, err)
	require.Equal(t, "app", rec.Network)

	recs, err := client.DNSList(context.Background(), "app")
	require.NoError(t, err)
	require.Len(t, recs, 1)

	_, err = client.DNSResolve(context.Background(), "missing", "app")
	require.Error(t, err)
}

func TestServer_InspectNotFound(t *testing.T) {
	client, _ := startTestServer(t)
	_, err := client.Inspect(context.Background(), "nonexistent")
	require.Error(t, err)
}

// --- Run with HealthCheck and Restart ---

func TestServer_Run_WithHealthCheckAndRestart(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img",
		Memory:    "256M",
		HealthCheck: &api.HealthCheckSpec{
			Type:     "tcp",
			Port:     8080,
			Interval: 5,
			Timeout:  3,
			Retries:  3,
		},
		Restart: &api.RestartSpec{
			Policy:     "on-failure",
			MaxRetries: 5,
		},
	})
	require.NoError(t, err)

	detail, err := client.Inspect(context.Background(), info.ID)
	require.NoError(t, err)
	require.Equal(t, "on-failure", detail.RestartPolicy)
	require.Equal(t, "starting", detail.Health)
}

func TestServer_Run_WithVolumes(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img",
		Memory:    "256M",
		Volumes: []api.VolumeMountSpec{
			{DiskPath: "/path/to/disk.img", GuestPath: "/mnt/data", ReadOnly: true},
		},
	})
	require.NoError(t, err)

	detail, err := client.Inspect(context.Background(), info.ID)
	require.NoError(t, err)
	require.Len(t, detail.Volumes, 1)
	require.Equal(t, "/mnt/data", detail.Volumes[0].GuestPath)
	require.True(t, detail.Volumes[0].ReadOnly)
}

// --- Daemon ---

func TestServer_DaemonVersion(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "jerboad.sock")
	mgr := vm.NewQEMUManager("fake-qemu")
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)

	srv, err := apiserver.NewServer(mgr, netStore, nil, socketPath, nil, "test-v1.2.3", nil)
	require.NoError(t, err)

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

	version, err := client.DaemonVersion(context.Background())
	require.NoError(t, err)
	require.Equal(t, "test-v1.2.3", version)
}

func TestServer_DaemonShutdown(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "jerboad.sock")
	mgr := vm.NewQEMUManager("fake-qemu")
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)

	var shutdownCalled atomic.Bool
	shutdownFn := func() { shutdownCalled.Store(true) }

	srv, err := apiserver.NewServer(mgr, netStore, nil, socketPath, shutdownFn, "", nil)
	require.NoError(t, err)

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

	require.NoError(t, client.Shutdown(context.Background()))
	require.Eventually(t, shutdownCalled.Load, 2*time.Second, 10*time.Millisecond)
}

// --- Network ---

func TestServer_NetworkCreate(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.NetworkCreate(context.Background(), "testnet", "", "")
	require.NoError(t, err)
	require.Equal(t, "testnet", info.Name)
	require.Equal(t, "bridge", info.Driver)
	require.NotEmpty(t, info.Subnet)
	require.NotEmpty(t, info.Gateway)
	require.NotEmpty(t, info.Bridge)
	require.NotEmpty(t, info.CreatedAt)
}

func TestServer_NetworkCreateWithSubnet(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.NetworkCreate(context.Background(), "custom", "10.200.0.0/24", "")
	require.NoError(t, err)
	require.Equal(t, "custom", info.Name)
	require.Equal(t, "10.200.0.0/24", info.Subnet)
}

func TestServer_NetworkCreateDuplicate(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.NetworkCreate(context.Background(), "dupnet", "", "")
	require.NoError(t, err)
	_, err = client.NetworkCreate(context.Background(), "dupnet", "", "")
	require.Error(t, err)
}

func TestServer_NetworkList(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.NetworkCreate(context.Background(), "net1", "", "")
	require.NoError(t, err)
	_, err = client.NetworkCreate(context.Background(), "net2", "", "")
	require.NoError(t, err)

	nets, err := client.NetworkList(context.Background())
	require.NoError(t, err)
	require.Len(t, nets, 2)
}

func TestServer_NetworkListEmpty(t *testing.T) {
	client, _ := startTestServer(t)

	nets, err := client.NetworkList(context.Background())
	require.NoError(t, err)
	require.Empty(t, nets)
}

func TestServer_NetworkGet(t *testing.T) {
	client, _ := startTestServer(t)

	created, err := client.NetworkCreate(context.Background(), "mynet", "", "")
	require.NoError(t, err)

	got, err := client.NetworkGet(context.Background(), "mynet")
	require.NoError(t, err)
	require.Equal(t, created.Name, got.Name)
	require.Equal(t, created.Subnet, got.Subnet)
}

func TestServer_NetworkGetNotFound(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.NetworkGet(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestServer_NetworkRemove(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.NetworkCreate(context.Background(), "todelete", "", "")
	require.NoError(t, err)

	require.NoError(t, client.NetworkRemove(context.Background(), "todelete"))

	_, err = client.NetworkGet(context.Background(), "todelete")
	require.Error(t, err)
}

func TestServer_NetworkRemoveNotFound(t *testing.T) {
	client, _ := startTestServer(t)

	err := client.NetworkRemove(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestServer_NetworkAllocateIP(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.NetworkCreate(context.Background(), "ipnet", "", "")
	require.NoError(t, err)

	ip, err := client.NetworkAllocateIP(context.Background(), "ipnet")
	require.NoError(t, err)
	require.NotEmpty(t, ip)
}

func TestServer_NetworkAllocateIPMultiple(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.NetworkCreate(context.Background(), "multinet", "", "")
	require.NoError(t, err)

	ip1, err := client.NetworkAllocateIP(context.Background(), "multinet")
	require.NoError(t, err)
	ip2, err := client.NetworkAllocateIP(context.Background(), "multinet")
	require.NoError(t, err)
	require.NotEqual(t, ip1, ip2, "allocated IPs should be unique")
}

func TestServer_NetworkAllocateIPNotFound(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.NetworkAllocateIP(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestServer_NetworkReleaseIP(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.NetworkCreate(context.Background(), "relnet", "", "")
	require.NoError(t, err)

	ip, err := client.NetworkAllocateIP(context.Background(), "relnet")
	require.NoError(t, err)

	require.NoError(t, client.NetworkReleaseIP(context.Background(), "relnet", ip))
}

func TestServer_Stats(t *testing.T) {
	client, _ := startTestServer(t)

	info, err := client.Run(context.Background(), api.RunParams{
		ImagePath: "test.img",
		Memory:    "256M",
		CPUs:      1,
	})
	require.NoError(t, err)

	stats, err := client.Stats(context.Background(), info.ID)
	require.NoError(t, err)
	require.Equal(t, info.ID, stats.ID)
	require.NotEmpty(t, stats.State)
	require.NotEmpty(t, stats.Timestamp)
}

func TestServer_StatsNotFound(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.Stats(context.Background(), "nonexistent")
	require.Error(t, err)
}

type fakeClusterLister struct {
	members []apiserver.ClusterMember
}

func (f *fakeClusterLister) Members() []apiserver.ClusterMember {
	return f.members
}

func TestServer_NodeList_Disabled(t *testing.T) {
	client, _ := startTestServer(t)

	_, err := client.NodeList(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "Node.List")
}

func TestServer_NodeList_WithCluster(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "jerboad-node.sock")
	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(fakeQEMUCmd(true)))
	netStore, err := network.NewStore(t.TempDir())
	require.NoError(t, err)

	lister := &fakeClusterLister{
		members: []apiserver.ClusterMember{
			{ID: "node-1", Addr: "10.0.0.1:7946", Status: "alive", VMCount: 3, CPUCap: 8, MemCap: 16 * 1024 * 1024 * 1024, LastSeen: time.Now()},
			{ID: "node-2", Addr: "10.0.0.2:7946", Status: "suspect", VMCount: 1, CPUCap: 4, MemCap: 8 * 1024 * 1024 * 1024, LastSeen: time.Now()},
		},
	}

	srv, err := apiserver.NewServer(mgr, netStore, nil, socketPath, nil, "", lister)
	require.NoError(t, err)

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

	resp, err := client.NodeList(context.Background())
	require.NoError(t, err)
	require.Len(t, resp.Nodes, 2)
	require.Equal(t, "node-1", resp.Nodes[0].ID)
	require.Equal(t, "alive", resp.Nodes[0].Status)
	require.Equal(t, "suspect", resp.Nodes[1].Status)
}
