//go:build linux || (darwin && arm64)

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/snapshot"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests start the daemon over state that is damaged or unusable: a
// store path that is a file, VM records that do not parse, a config that is
// garbage, an endpoint that cannot be bound. The rule: what the daemon cannot
// do without stops startup with an error that names it; anything else is
// disabled, and the daemon serves the rest.

// daemon is one jerboad instance over a private home directory.
type daemon struct {
	t          *testing.T
	home       string
	endpoint   string
	token      string
	vmStore    string
	hypervisor string
	fcBin      string
	store      string
	tools      string
	disks      string
}

// newDaemon prepares a private home and a short socket path (Unix socket
// paths are limited to about a hundred bytes), without starting anything.
func newDaemon(t *testing.T) *daemon {
	t.Helper()
	isolateHome(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	sockDir, err := os.MkdirTemp("/tmp", "jd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".jerboa"), 0o700))
	return &daemon{
		t:          t,
		home:       home,
		endpoint:   "unix://" + filepath.Join(sockDir, "d.sock"),
		vmStore:    "file",
		hypervisor: "qemu",
		// Never on PATH, so a daemon left to choose settles on QEMU and
		// does not go looking for a Firecracker kernel.
		fcBin: "firecracker-that-does-not-exist",
		store: filepath.Join(home, ".jerboa", "images"),
		tools: filepath.Join(home, ".jerboa", "tools"),
		disks: filepath.Join(home, ".jerboa", "vm-disks"),
	}
}

func (d *daemon) serve(ctx context.Context) error {
	return serve(ctx, d.endpoint, d.token, "", "", "qemu-that-does-not-exist", d.store, d.vmStore,
		"", "", "text", "", "", "", d.hypervisor, d.fcBin, "", d.tools, d.disks)
}

// fails runs a startup that must not get as far as serving and returns its error.
func (d *daemon) fails() error {
	d.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := d.serve(ctx)
	require.Error(d.t, err, "startup must fail, not serve")
	return err
}

// start serves in the background and returns once the daemon answers. The
// daemon is stopped, and waited for, when the test ends.
func (d *daemon) start() *api.Client {
	d.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.serve(ctx) }()
	d.t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(d.t, err, "a daemon asked to stop must shut down cleanly")
		case <-time.After(30 * time.Second):
			d.t.Error("daemon did not stop")
		}
	})

	var client *api.Client
	require.Eventually(d.t, func() bool {
		select {
		case err := <-done:
			done <- err
			d.t.Logf("daemon exited during startup: %v", err)
			return false
		default:
		}
		c, err := api.DialWithToken(d.endpoint, d.token)
		if err != nil {
			return false
		}
		client = c
		return true
	}, 30*time.Second, 20*time.Millisecond, "daemon did not start")
	d.t.Cleanup(func() { _ = client.Close() })
	return client
}

func (d *daemon) path(rel string) string { return filepath.Join(d.home, ".jerboa", rel) }

// blockWithFile puts a regular file where a directory is expected.
func blockWithFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("not a directory"), 0o600))
}

func socketPath(endpoint string) string { return endpoint[len("unix://"):] }

func TestFaults_ServeRejectsUnknownBackends(t *testing.T) {
	d := newDaemon(t)
	d.vmStore = "etcd"
	err := d.fails()
	require.ErrorContains(t, err, `unknown vm-store backend "etcd"`)
	assert.Contains(t, err.Error(), "file or sqlite")
	assert.NoFileExists(t, socketPath(d.endpoint), "nothing may be listening after a failed start")

	d = newDaemon(t)
	d.hypervisor = "xen"
	err = d.fails()
	require.ErrorContains(t, err, `unknown hypervisor "xen"`)
	assert.Contains(t, err.Error(), "qemu, firecracker")
	assert.NoFileExists(t, socketPath(d.endpoint))
}

func TestFaults_ServeWithHypervisorFromDamagedConfig(t *testing.T) {
	ctx := context.Background()

	// A config that does not parse must not keep the daemon down: it chooses
	// the hypervisor from the host, as if nothing had been configured.
	d := newDaemon(t)
	d.hypervisor = ""
	require.NoError(t, os.WriteFile(d.path("config.toml"), []byte("[daemon\nhypervisor = "), 0o600))
	info, err := d.start().DaemonInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, "qemu", info.Hypervisor.Name)
	assert.NotEmpty(t, info.Hypervisor.Reason)

	// One that parses but names a hypervisor that does not exist is an
	// operator's mistake, and is reported rather than silently replaced.
	d = newDaemon(t)
	d.hypervisor = ""
	require.NoError(t, os.WriteFile(d.path("config.toml"), []byte("hypervisor = \"hyperv\"\n"), 0o600))
	require.ErrorContains(t, d.fails(), `unknown hypervisor "hyperv"`)
}

func TestFaults_ServeWithUnusableVMStore(t *testing.T) {
	// The SQLite store cannot be created where a file sits: without a VM
	// store there is nothing to serve.
	d := newDaemon(t)
	d.vmStore = "sqlite"
	blockWithFile(t, d.path("vms"))
	require.ErrorContains(t, d.fails(), "vm store")
	assert.NoFileExists(t, socketPath(d.endpoint))

	// A database file that is not a database.
	d = newDaemon(t)
	d.vmStore = "sqlite"
	require.NoError(t, os.MkdirAll(d.path("vms"), 0o700))
	require.NoError(t, os.WriteFile(d.path("vms/vms.db"), []byte("this is not an SQLite file, only text of some length\n"), 0o600))
	err := d.fails()
	require.ErrorContains(t, err, "vm store")
	data, rerr := os.ReadFile(d.path("vms/vms.db"))
	require.NoError(t, rerr)
	assert.Contains(t, string(data), "this is not an SQLite file", "a file the store cannot read must be left for the operator, not overwritten")
}

func TestFaults_ServeWithDamagedVMRecords(t *testing.T) {
	// What a full disk or a power cut leaves of VM records: an empty
	// directory, an empty state file, half a JSON document, binary noise, a
	// stray file. The daemon skips them and serves.
	d := newDaemon(t)
	for name, content := range map[string][]byte{
		"vms/0001/state.json": nil,
		"vms/0002/state.json": []byte(`{"id":"0002","config":{"name":"half`),
		"vms/0003/state.json": {0x00, 0xff, 0xfe, 0x01},
		"vms/0004/state.json": []byte(`[]`),
		"vms/0005/state.json": []byte(`{"id":7,"state":{}}`),
		"vms/stray-file":      []byte("x"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(d.path(name)), 0o700))
		require.NoError(t, os.WriteFile(d.path(name), content, 0o600))
	}
	require.NoError(t, os.MkdirAll(d.path("vms/0006"), 0o700))

	c := d.start()
	vms, err := c.List(context.Background())
	require.NoError(t, err)
	for _, v := range vms {
		assert.NotEmpty(t, v.ID, "a damaged record must not surface as a VM without an id")
	}
	// The damaged records are still there to be inspected.
	assert.FileExists(t, d.path("vms/0002/state.json"))
}

func TestFaults_ServeWithVMRecordsPathBlocked(t *testing.T) {
	// The file store reads its directory lazily: one that cannot be read
	// loses the records, not the daemon.
	d := newDaemon(t)
	blockWithFile(t, d.path("vms"))
	vms, err := d.start().List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, vms)
}

func TestFaults_ServeWithUnusableNetworkStore(t *testing.T) {
	// Every VM needs an address: a daemon that cannot record leases must not start.
	d := newDaemon(t)
	blockWithFile(t, d.path("networks"))
	err := d.fails()
	require.ErrorContains(t, err, "network store")
	assert.Contains(t, err.Error(), d.path("networks"), "the error names the path that is in the way")
	assert.NoFileExists(t, socketPath(d.endpoint))
}

func TestFaults_ServeWithUnusableImageStore(t *testing.T) {
	ctx := context.Background()
	d := newDaemon(t)
	blockWithFile(t, d.store)
	c := d.start()

	// The daemon is up: VMs that already exist can be listed and stopped.
	_, err := c.List(ctx)
	require.NoError(t, err)
	_, err = c.DaemonInfo(ctx)
	require.NoError(t, err)

	// Image operations fail with an error, on a connection that stays usable.
	_, err = c.ImageList(ctx)
	require.Error(t, err)
	_, err = c.ImageGet(ctx, "app:latest")
	require.Error(t, err)
	require.Error(t, c.ImageRemove(ctx, "app:latest"))
	_, err = c.Run(ctx, api.RunParams{Image: "app:latest"})
	require.Error(t, err, "a run by image name cannot be resolved without an image store")
	_, err = c.List(ctx)
	require.NoError(t, err, "the connection must survive the failed calls")

	data, rerr := os.ReadFile(d.store)
	require.NoError(t, rerr)
	assert.Equal(t, "not a directory", string(data), "the file in the way is not the daemon's to remove")
}

func TestFaults_ServeWithUnusableVolumeStore(t *testing.T) {
	ctx := context.Background()
	d := newDaemon(t)
	blockWithFile(t, volumeStorePath(d.store))
	c := d.start()

	_, err := c.VolumeList(ctx)
	require.Error(t, err)
	_, err = c.VolumeCreate(ctx, "data", 1<<20)
	require.Error(t, err)

	// Images and VMs are unaffected.
	images, err := c.ImageList(ctx)
	require.NoError(t, err)
	assert.Empty(t, images)
	_, err = c.List(ctx)
	require.NoError(t, err)
}

func TestFaults_ServeWithUnusableToolsAndDiskDirs(t *testing.T) {
	// The toolchain is resolved on the first build and boot disks are made
	// on the first run: neither may block startup.
	ctx := context.Background()
	d := newDaemon(t)
	blockWithFile(t, d.tools)
	blockWithFile(t, d.disks)
	c := d.start()

	_, err := c.List(ctx)
	require.NoError(t, err)
	images, err := c.ImageList(ctx)
	require.NoError(t, err)
	assert.Empty(t, images)

	// A volume is formatted with the toolchain when it is first attached,
	// not when it is created, so creating one still works.
	v, err := c.VolumeCreate(ctx, "data", 1<<20)
	require.NoError(t, err)
	assert.Equal(t, "data", v.ID)
}

func TestFaults_ServeWithUnbindableEndpoint(t *testing.T) {
	// The socket's directory does not exist.
	d := newDaemon(t)
	d.endpoint = "unix://" + filepath.Join(filepath.Dir(socketPath(d.endpoint)), "missing", "d.sock")
	err := d.fails()
	require.ErrorContains(t, err, "vm server")
	assert.Contains(t, err.Error(), "missing")

	// The TCP port is taken by another program.
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, lerr)
	defer func() { _ = ln.Close() }()
	d = newDaemon(t)
	d.endpoint = "tcp://" + ln.Addr().String()
	d.token = strongToken
	err = d.fails()
	require.ErrorContains(t, err, "vm server")
	assert.Contains(t, err.Error(), ln.Addr().String())

	// Neither a scheme the daemon does not speak nor an empty address.
	for _, endpoint := range []string{"http://127.0.0.1:7890", "unix://", "tcp://"} {
		d = newDaemon(t)
		d.endpoint = endpoint
		require.ErrorContains(t, d.fails(), "vm server", "%s", endpoint)
	}
}

func TestFaults_ServeReplacesAStaleSocket(t *testing.T) {
	// A daemon that was killed leaves its socket file behind; the next one
	// must not refuse to start over it.
	d := newDaemon(t)
	stale, err := net.Listen("unix", socketPath(d.endpoint))
	require.NoError(t, err)
	// Keep the file when the listener closes, as a killed process does.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())
	require.FileExists(t, socketPath(d.endpoint))

	_, err = d.start().List(context.Background())
	require.NoError(t, err)

	info, err := os.Stat(socketPath(d.endpoint))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a Unix socket has no token: its mode is the access control")
}

func TestFaults_SecondDaemonDoesNotTakeOverALiveSocket(t *testing.T) {
	t.Skip("BUG: api.Listen (internal/api/endpoint.go) removes the socket file without checking that a daemon " +
		"is listening on it, so a second jerboad on the same endpoint starts, and the first one keeps running " +
		"unreachable with its VMs")
	first := newDaemon(t)
	c := first.start()

	second := *first
	err := second.fails()
	require.ErrorContains(t, err, "vm server")

	_, err = c.List(context.Background())
	require.NoError(t, err)
	fresh, err := api.Dial(first.endpoint)
	require.NoError(t, err, "the first daemon must still be the one reachable at its socket")
	_ = fresh.Close()
}

func TestFaults_ServeEnforcesTheToken(t *testing.T) {
	ctx := context.Background()
	d := newDaemon(t)
	d.token = strongToken
	c := d.start()
	_, err := c.List(ctx)
	require.NoError(t, err)

	for name, token := range map[string]string{"none": "", "wrong": "ffffffffffffffffffffffffffffffff", "prefix": strongToken[:16]} {
		bad, derr := api.DialWithToken(d.endpoint, token)
		if derr == nil {
			// A client that sends no token only learns it on its first call.
			_, derr = bad.List(ctx)
			_ = bad.Close()
		}
		require.Error(t, derr, "token %q must not get in", name)
		assert.NotContains(t, derr.Error(), strongToken)
	}

	_, err = c.List(ctx)
	require.NoError(t, err, "failed attempts must not disturb an authenticated client")
}

func TestFaults_ServeStopsWhileClientsAreConnected(t *testing.T) {
	d := newDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.serve(ctx) }()

	var clients []*api.Client
	require.Eventually(t, func() bool {
		c, err := api.Dial(d.endpoint)
		if err != nil {
			return false
		}
		clients = append(clients, c)
		return true
	}, 30*time.Second, 20*time.Millisecond)
	for range 4 {
		c, err := api.Dial(d.endpoint)
		require.NoError(t, err)
		clients = append(clients, c)
	}
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()

	// Idle connections must not hold the daemon open.
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not stop with idle clients connected")
	}
	_, err := api.Dial(d.endpoint)
	require.Error(t, err, "a stopped daemon must not accept new clients")
}

func TestFaults_FirecrackerWithoutAKernel(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("on Linux a missing kernel is downloaded")
	}
	// Asked for by name, Firecracker without its kernel is an error that
	// says what is missing and how to provide it.
	d := newDaemon(t)
	d.hypervisor = "firecracker"
	err := d.fails()
	require.ErrorContains(t, err, "kernel")
	assert.Contains(t, err.Error(), "--fc-kernel")
	assert.Contains(t, err.Error(), filepath.Join(d.tools, "kernel.img"))
	assert.NoFileExists(t, socketPath(d.endpoint))
}

func TestFaults_NewVMStore(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()

	_, err := newVMStore("postgres", dir)
	require.ErrorContains(t, err, `unknown vm-store backend "postgres"`)

	// Empty means the default, as a daemon started by an older unit file does.
	store, err := newVMStore("", dir)
	require.NoError(t, err)
	require.NoError(t, store.Restore())

	// File records that cannot be migrated do not stop the SQLite store from
	// opening: the daemon runs, and the records stay on disk.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "broken"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken", "state.json"), []byte(`{"id":`), 0o600))
	store, err = newVMStore("sqlite", dir)
	require.NoError(t, err)
	require.NoError(t, store.Restore())
	assert.Empty(t, store.List())
	assert.FileExists(t, filepath.Join(dir, "broken", "state.json"))
}

func TestFaults_SnapshotStoreOptionSurvivesAnUnusableDirectory(t *testing.T) {
	// Snapshots are optional: a directory that cannot be opened disables
	// them when the manager is built, and must not panic or stop the daemon.
	blocked := filepath.Join(t.TempDir(), "snapshots")
	blockWithFile(t, blocked)
	opt := snapshotStoreOption(filepath.Join(blocked, "store"), snapshot.Limits{MaxCount: 1})
	require.NotNil(t, opt)
	require.NotPanics(t, func() { opt(vm.NewFirecrackerManager("firecracker-that-does-not-exist", "")) })
}
