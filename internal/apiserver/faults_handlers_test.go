//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/AitorConS/jerboa/internal/scheduler"
	"github.com/AitorConS/jerboa/internal/snapshot"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// paramMethods are the request/response methods that take parameters.
var paramMethods = []string{
	"VM.Run", "VM.Start", "VM.Stop", "VM.Kill", "VM.Signal", "VM.Remove", "VM.Get", "VM.Logs", "VM.Inspect", "VM.Stats",
	"VM.SnapshotCreate", "VM.SnapshotRestore", "Snapshot.Inspect", "Snapshot.Remove",
	"Image.Get", "Image.Remove", "Image.Recipe", "Image.SBOM", "Image.Tag", "Image.Push", "Image.Pull",
	"Network.Create", "Network.Get", "Network.Remove",
	"Volume.Create", "Volume.Get", "Volume.Remove",
	"DNS.Resolve", "DNS.ResolveAll", "DNS.List",
	"System.Prune", "System.Check",
}

// fullEnv has every optional feature switched on, so a handler cannot answer
// "disabled" before it looks at its parameters.
func fullEnv(t *testing.T) *faultEnv {
	t.Helper()
	e := newFaultEnv(t)
	blobs, err := cas.New(t.TempDir())
	require.NoError(t, err)
	e.s.recipes = blobs
	e.s.registryMkfs = func(context.Context, string) (string, error) { return "mkfs", nil }
	e.s.mgr = &snapshotMock{MockManager: e.mgr}
	return e
}

// Parameters that are not what the method expects — the wrong JSON type, cut
// off, missing — are refused as invalid parameters. They never reach a store
// or the hypervisor, and never take the daemon down.
func TestHandlers_MalformedParams(t *testing.T) {
	payloads := map[string]string{
		"array":     `[1,2,3]`,
		"string":    `"id"`,
		"number":    `42`,
		"truncated": `{"id":"abc"`,
		"missing":   ``,
		"mistyped":  `{"id":{"nested":true},"name":7,"ref":[],"network":false,"reference":1,"source":2,"all":"yes","deep":"no"}`,
	}
	for _, method := range paramMethods {
		for kind, payload := range payloads {
			if kind == "missing" && (method == "DNS.List" || method == "System.Prune" || method == "System.Check") {
				continue // every parameter of these is optional
			}
			t.Run(method+"/"+kind, func(t *testing.T) {
				e := fullEnv(t)
				before := listTree(t, filepath.Dir(e.netRoot))
				res, rpcErr := e.s.dispatch(context.Background(), &api.Request{ID: 7, Method: method, Params: json.RawMessage(payload)}, nil, nil)
				require.Nil(t, res)
				requireRPCError(t, rpcErr, -32602, "")
				require.Empty(t, e.mgr.List())
				require.Equal(t, before, listTree(t, filepath.Dir(e.netRoot)), "a refused request changed the daemon's data")
			})
		}
	}
}

// null is valid JSON for any parameter object; it must be treated as "no
// parameters given" and not dereferenced.
func TestHandlers_NullParams(t *testing.T) {
	for _, method := range paramMethods {
		if method == "VM.Run" {
			// A run that names no image is refused by the VM manager's own
			// validation, which the mock manager used here does not perform.
			continue
		}
		t.Run(method, func(t *testing.T) {
			e := fullEnv(t)
			require.NotPanics(t, func() {
				res, rpcErr := e.s.dispatch(context.Background(), &api.Request{ID: 7, Method: method, Params: json.RawMessage(`null`)}, nil, nil)
				switch method {
				case "DNS.List", "System.Prune", "System.Check":
					require.Nil(t, rpcErr, "every parameter of %s is optional", method)
				default:
					require.NotNil(t, rpcErr, "%s accepted a request that names nothing: %v", method, res)
					requireRPCError(t, rpcErr, rpcErr.Code, "")
				}
			})
			require.Empty(t, e.mgr.List())
			e.requireNoLeak()
		})
	}
}

// Requests naming something that does not exist get an error that says so,
// and change nothing.
func TestHandlers_UnknownTargets(t *testing.T) {
	cases := []struct {
		method string
		params any
		code   int
		msg    string
	}{
		{"VM.Start", api.IDParams{ID: "ghost"}, -32000, "not found"},
		{"VM.Stop", api.StopParams{ID: "ghost"}, -32000, "not found"},
		{"VM.Stop", api.StopParams{ID: "ghost", Force: true}, -32000, "not found"},
		{"VM.Kill", api.IDParams{ID: "ghost"}, -32000, "not found"},
		{"VM.Remove", api.IDParams{ID: "ghost"}, -32000, "not found"},
		{"VM.Get", api.IDParams{ID: "ghost"}, -32000, "not found"},
		{"VM.Logs", api.IDParams{ID: "ghost"}, -32000, "not found"},
		{"VM.Inspect", api.IDParams{ID: "ghost"}, -32000, "not found"},
		{"VM.Stats", api.IDParams{ID: "ghost"}, -32000, "not found"},
		{"VM.Start", api.IDParams{}, -32000, "not found"},
		{"VM.Remove", api.IDParams{ID: "../../etc/passwd"}, -32000, "not found"},
		{"VM.SnapshotRestore", api.SnapshotParams{ID: "ghost", Name: "snap"}, -32000, "not found"},
		{"VM.SnapshotCreate", api.SnapshotParams{ID: "ghost"}, -32602, "required"},
		{"VM.SnapshotRestore", api.SnapshotParams{Name: "snap"}, -32602, "required"},
		{"Snapshot.Inspect", api.SnapshotParams{}, -32602, "required"},
		{"Snapshot.Remove", api.SnapshotParams{}, -32602, "required"},
		{"Image.Get", map[string]string{"ref": "ghost:latest"}, api.CodeNotFound, "ghost:latest"},
		{"Image.Get", map[string]string{"ref": ""}, api.CodeNotFound, ""},
		{"Image.Remove", map[string]string{"ref": "ghost:latest"}, -32000, "ghost:latest"},
		{"Image.Remove", map[string]string{"ref": "../../etc"}, -32000, ""},
		{"Image.SBOM", map[string]string{"ref": "ghost:latest"}, -32000, "ghost:latest"},
		{"Image.Recipe", map[string]string{"ref": "ghost:latest"}, -32000, "ghost:latest"},
		{"Image.Tag", api.ImageTagParams{Source: "ghost:latest", Name: "copy", Tag: "v1"}, -32000, "ghost:latest"},
		{"Image.Tag", api.ImageTagParams{Source: "ghost:latest", Name: "copy"}, -32602, "required"},
		{"Image.Tag", api.ImageTagParams{Name: "copy", Tag: "v1"}, -32602, "required"},
		{"Image.Push", api.ImagePushParams{Reference: "not a reference"}, -32000, ""},
		{"Image.Push", api.ImagePushParams{Reference: "registry.example/acme/ghost:v1"}, -32000, "not in the image store"},
		{"Image.Pull", api.ImagePullParams{Reference: "not a reference"}, -32000, ""},
		{"Image.Pull", api.ImagePullParams{Reference: "registry.example/acme/ghost:v1", Platform: "plan9/mips"}, -32000, "plan9/mips"},
		{"Network.Get", map[string]string{"name": "ghost"}, -32000, "not found"},
		{"Network.Get", map[string]string{"name": "../volumes"}, -32000, ""},
		{"Network.Remove", map[string]string{"name": "ghost"}, -32000, "not found"},
		{"Network.Remove", map[string]string{"name": ".."}, -32000, ""},
		{"Network.Remove", map[string]string{"name": ""}, -32000, ""},
		{"Network.Create", api.NetworkCreateParams{Name: faultNet}, -32000, "already exists"},
		{"Network.Create", api.NetworkCreateParams{Name: "../escape"}, -32000, ""},
		{"Network.Create", api.NetworkCreateParams{Name: ""}, -32000, ""},
		{"Network.Create", api.NetworkCreateParams{Name: "bad", Subnet: "not-a-cidr"}, -32000, ""},
		{"Network.Create", api.NetworkCreateParams{Name: "bad", Subnet: "10.100.0.128/25"}, -32000, "overlap"},
		{"Volume.Get", api.VolumeCreateParams{Name: "ghost"}, api.CodeNotFound, "ghost"},
		{"Volume.Get", api.VolumeCreateParams{}, -32602, "invalid volume parameters"},
		{"Volume.Create", api.VolumeCreateParams{Name: "data", SizeBytes: -1}, -32602, "invalid volume parameters"},
		{"Volume.Create", api.VolumeCreateParams{Name: "../escape", SizeBytes: 1 << 20}, -32000, ""},
		{"Volume.Remove", api.VolumeRemoveParams{Name: "ghost"}, api.CodeNotFound, "ghost"},
		{"Volume.Remove", api.VolumeRemoveParams{Name: "../networks"}, -32000, ""},
		{"DNS.Resolve", api.DNSResolveParams{Name: "ghost", Network: faultNet}, -32000, "ghost"},
		{"DNS.ResolveAll", api.DNSResolveParams{Name: "ghost", Network: faultNet}, -32000, "ghost"},
		{"VM.Signal", api.SignalParams{ID: "ghost", Signal: "SIGBOGUS"}, -32602, "unknown signal"},
		{"VM.Signal", api.SignalParams{ID: "ghost", Signal: ""}, -32602, "unknown signal"},
		{"VM.Signal", api.SignalParams{ID: "ghost", Signal: "$(reboot)"}, -32602, "unknown signal"},
		{"Bogus.Method", struct{}{}, -32601, "Bogus.Method"},
		{"", struct{}{}, -32601, "method not found"},
		{"vm.run", api.RunParams{ImagePath: "/x"}, -32601, "method not found"},
	}
	e := fullEnv(t)
	root := filepath.Dir(e.netRoot)
	before := listTree(t, root)
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%v", tc.method, tc.params), func(t *testing.T) {
			res, rpcErr := e.call(tc.method, tc.params)
			require.Nil(t, res)
			requireRPCError(t, rpcErr, tc.code, tc.msg)
			require.Empty(t, e.mgr.List())
			require.Equal(t, before, listTree(t, root), "a refused request changed the daemon's data")
		})
	}
	// Nothing escaped the stores either.
	require.NoDirExists(t, filepath.Join(root, "escape"))
	require.NoFileExists(t, filepath.Join(root, "escape"))
}

// Addresses are leased and released only by the VM lifecycle. The daemon used
// to answer Network.AllocateIP and Network.ReleaseIP, which no client called,
// which did not check whether a registered VM held the address and which ran
// outside resourceMu: any client could give back a running VM's address and
// have it handed to the next VM. They are gone, and the leases stay exactly
// those of the registered VMs.
func TestHandlers_LeasesAreNotAClientOperation(t *testing.T) {
	e := newFaultEnv(t)
	_, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet})
	require.Nil(t, rpcErr)
	held := e.leases()
	require.Len(t, held, 1)
	for _, ip := range append(held, faultGateway) {
		_, rpcErr := e.call("Network.ReleaseIP", map[string]string{"network": faultNet, "ip": ip})
		requireRPCError(t, rpcErr, -32601, "method not found")
	}
	_, rpcErr = e.call("Network.AllocateIP", map[string]string{"network": faultNet})
	requireRPCError(t, rpcErr, -32601, "method not found")
	require.Equal(t, held, e.leases())
	e.requireNoLeak()

	// The next VM gets an address of its own, not the one web holds.
	info, rpcErr := e.run(api.RunParams{Name: "api", NetworkName: faultNet})
	require.Nil(t, rpcErr)
	v, err := e.mgr.Get(info.ID)
	require.NoError(t, err)
	require.NotContains(t, append(held, faultGateway), v.Cfg.IPAddress)
	// Nor can a VM ask for the gateway's address by name.
	_, rpcErr = e.run(api.RunParams{Name: "squatter", NetworkName: faultNet, IPAddress: faultGateway})
	require.NotNil(t, rpcErr, "a VM was started on the gateway's address")
}

func TestHandlers_StoresDisabled(t *testing.T) {
	mgr := vm.NewMockManager()
	s := &Server{mgr: mgr, resolver: scheduler.NewResolver(mgr)}
	e := &faultEnv{t: t, s: s, mgr: mgr}
	cases := []struct {
		method string
		params any
		code   int
		msg    string
	}{
		{"Image.List", struct{}{}, -32601, "image store disabled"},
		{"Image.Get", map[string]string{"ref": "a:b"}, -32601, "image store disabled"},
		{"Image.Remove", map[string]string{"ref": "a:b"}, -32601, "image store disabled"},
		{"Image.SBOM", map[string]string{"ref": "a:b"}, -32601, "image store disabled"},
		{"Image.Recipe", map[string]string{"ref": "a:b"}, -32601, "recipes disabled"},
		{"Image.Tag", api.ImageTagParams{Source: "a:b", Name: "c", Tag: "d"}, -32601, "image store disabled"},
		{"Image.Push", api.ImagePushParams{Reference: "r.example/a/b:c"}, -32601, "disabled on this daemon"},
		{"Image.Pull", api.ImagePullParams{Reference: "r.example/a/b:c"}, -32601, "disabled on this daemon"},
		{"Volume.Create", api.VolumeCreateParams{Name: "data"}, -32601, "volume store disabled"},
		{"Volume.Get", api.VolumeCreateParams{Name: "data"}, -32601, "volume store disabled"},
		{"Volume.List", struct{}{}, -32601, "volume store disabled"},
		{"Volume.Remove", api.VolumeRemoveParams{Name: "data"}, -32000, "volume store disabled"},
		{"VM.Run", api.RunParams{Image: "web:latest"}, -32000, "image store disabled"},
		{"VM.Run", api.RunParams{ImagePath: "/x.img", Volumes: []api.VolumeMountSpec{{Name: "data", GuestPath: "/data"}}}, -32000, "volume store disabled"},
		{"Node.List", struct{}{}, -32000, "cluster is disabled"},
		{"VM.SnapshotCreate", api.SnapshotParams{ID: "a", Name: "b"}, -32000, "snapshots require"},
		{"VM.SnapshotRestore", api.SnapshotParams{ID: "a", Name: "b"}, -32000, "snapshots require"},
		{"Snapshot.List", struct{}{}, -32000, "snapshots require"},
		{"Snapshot.Inspect", api.SnapshotParams{Name: "b"}, -32000, "snapshots require"},
		{"Snapshot.Remove", api.SnapshotParams{Name: "b"}, -32000, "snapshots require"},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			res, rpcErr := e.call(tc.method, tc.params)
			require.Nil(t, res)
			requireRPCError(t, rpcErr, tc.code, tc.msg)
			require.Empty(t, mgr.List())
		})
	}

	// What needs no store still works.
	res, rpcErr := e.call("VM.Run", api.RunParams{ImagePath: "/x.img", Name: "web"})
	require.Nil(t, rpcErr)
	info, ok := res.(api.VMInfo)
	require.True(t, ok)
	for _, method := range []string{"VM.List", "Daemon.Version", "System.DF", "DNS.List"} {
		_, rpcErr := e.call(method, struct{}{})
		require.Nil(t, rpcErr, method)
	}
	require.NoError(t, mgr.Stop(context.Background(), info.ID))
	_, rpcErr = e.call("VM.Remove", api.IDParams{ID: info.ID})
	require.Nil(t, rpcErr, "a VM without a network is removable without a network store")
}

// The streamed methods refuse too, without waiting for a stream that is not
// coming to be useful: the client's upload is drained and one error is sent.
func TestHandlers_StreamedMethodsDisabled(t *testing.T) {
	mgr := vm.NewMockManager()
	s := &Server{mgr: mgr}
	stream := func() *strings.Reader { return strings.NewReader(string(framed([]byte("ignored upload"), 4))) }

	resp := capture(t, func(conn net.Conn) {
		s.handleBuild(context.Background(), rawParams(t, api.BuildParams{Name: "x", Program: "app"}), api.NewFrameReader(stream()), conn, 3)
	})
	require.NotNil(t, resp)
	require.EqualValues(t, 3, resp.ID)
	requireRPCError(t, resp.Error, -32601, "image build disabled")

	resp = capture(t, func(conn net.Conn) {
		s.handleVolumeSeed(context.Background(), rawParams(t, api.VolumeSeedParams{VolumeName: "v", DiskPath: "/d", Label: "v"}), api.NewFrameReader(stream()), conn, 4)
	})
	require.NotNil(t, resp)
	requireRPCError(t, resp.Error, -32601, "volume store disabled")

	resp = capture(t, func(conn net.Conn) {
		s.handleVolumeImport(context.Background(), rawParams(t, api.VolumeImportParams{Name: "v", Label: "v", SizeBytes: 4}), api.NewFrameReader(stream()), conn, 5)
	})
	require.NotNil(t, resp)
	requireRPCError(t, resp.Error, -32601, "volume store disabled")
}

// A store that fails underneath the handler (its directory is gone or
// unreadable) yields an error for that request, not a crash.
func TestHandlers_StoreDirectoryGone(t *testing.T) {
	e := newFaultEnv(t)
	_, err := e.vols.Create("data", 1<<20)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(e.netRoot))
	require.NoError(t, os.RemoveAll(e.volRoot))
	require.NoError(t, os.RemoveAll(e.imgRoot))

	for _, tc := range []struct {
		method string
		params any
	}{
		{"Network.List", struct{}{}},
		{"Network.Get", map[string]string{"name": faultNet}},
		{"Network.Remove", map[string]string{"name": faultNet}},
		{"Volume.List", struct{}{}},
		{"Volume.Get", api.VolumeCreateParams{Name: "data"}},
		{"Volume.Remove", api.VolumeRemoveParams{Name: "data"}},
		{"Image.Get", map[string]string{"ref": "web:latest"}},
		{"VM.Run", api.RunParams{ImagePath: "/x.img", NetworkName: faultNet}},
		{"VM.Run", api.RunParams{Image: "web:latest"}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			var rpcErr *api.RPCError
			require.NotPanics(t, func() { _, rpcErr = e.call(tc.method, tc.params) })
			require.NotNil(t, rpcErr)
			requireRPCError(t, rpcErr, rpcErr.Code, "")
			require.Empty(t, e.mgr.List())
		})
	}
	// Listing images of a store with nothing left in it is an empty list.
	res, rpcErr := e.call("Image.List", struct{}{})
	if rpcErr == nil {
		require.Empty(t, res)
	}
}

// Removing things that are in use is refused, naming who uses them.
func TestHandlers_RemoveInUse(t *testing.T) {
	e := newFaultEnv(t)
	vol, err := e.vols.Create("data", 1<<20)
	require.NoError(t, err)
	e.s.EnableVolumeFormatResolver(func(context.Context) (volume.Formatter, error) {
		return func(_ context.Context, disk, _ string, _ int64) error {
			return os.WriteFile(disk, append([]byte("NVMTFS"), make([]byte, 1<<16)...), 0o600)
		}, nil
	})
	info, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet, Volumes: []api.VolumeMountSpec{{Name: "data", GuestPath: "/data"}}})
	require.Nil(t, rpcErr)

	_, rpcErr = e.call("Network.Remove", map[string]string{"name": faultNet})
	requireRPCError(t, rpcErr, -32000, "in use by 1 running VM")
	_, rpcErr = e.call("Volume.Remove", api.VolumeRemoveParams{Name: "data"})
	requireRPCError(t, rpcErr, -32000, info.ID)
	_, rpcErr = e.call("Volume.Remove", api.VolumeRemoveParams{Name: "data", DiskPath: "/etc/passwd"})
	requireRPCError(t, rpcErr, -32000, "mismatch")
	require.FileExists(t, vol.DiskPath)
	_, err = e.nets.Get(faultNet)
	require.NoError(t, err)

	// A stopped VM still references its volume; only removing the VM frees it.
	require.NoError(t, e.mgr.Stop(context.Background(), info.ID))
	_, rpcErr = e.call("Volume.Remove", api.VolumeRemoveParams{Name: "data"})
	requireRPCError(t, rpcErr, -32000, info.ID)
	_, rpcErr = e.call("VM.Remove", api.IDParams{ID: info.ID})
	require.Nil(t, rpcErr)
	_, rpcErr = e.call("Volume.Remove", api.VolumeRemoveParams{Name: "data"})
	require.Nil(t, rpcErr)
	require.NoFileExists(t, vol.DiskPath)
}

// Snapshot backends fail in their own ways; each failure is passed on as an
// error and a restore that is refused does not touch the VM.
func TestHandlers_SnapshotFailures(t *testing.T) {
	e := newFaultEnv(t)
	sm := &failingSnapshots{MockManager: e.mgr}
	e.s.mgr = sm

	for _, tc := range []struct {
		method string
		params any
	}{
		{"VM.SnapshotCreate", api.SnapshotParams{ID: "x", Name: "s"}},
		{"Snapshot.List", struct{}{}},
		{"Snapshot.Inspect", api.SnapshotParams{Name: "s"}},
		{"Snapshot.Remove", api.SnapshotParams{Name: "s"}},
	} {
		_, rpcErr := e.call(tc.method, tc.params)
		requireRPCError(t, rpcErr, -32000, errInjected.Error())
	}

	// Restore: the VM must be stopped, on a network that still matches.
	info, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet})
	require.Nil(t, rpcErr)
	_, rpcErr = e.call("VM.SnapshotRestore", api.SnapshotParams{ID: info.ID, Name: "s"})
	requireRPCError(t, rpcErr, -32000, "requires a stopped VM")
	require.NoError(t, e.mgr.Stop(context.Background(), info.ID))

	_, rpcErr = e.call("VM.SnapshotRestore", api.SnapshotParams{ID: info.ID, Name: "s"})
	requireRPCError(t, rpcErr, -32000, errInjected.Error())
	assert.Equal(t, 1, sm.restores, "the backend is asked once the checks pass")
	e.requireNoLeak()

	bare, err := e.mgr.Create(context.Background(), vm.Config{Name: "bare"})
	require.NoError(t, err)
	require.NoError(t, e.mgr.Start(context.Background(), bare.ID))
	require.NoError(t, e.mgr.Stop(context.Background(), bare.ID))
	_, rpcErr = e.call("VM.SnapshotRestore", api.SnapshotParams{ID: bare.ID, Name: "s"})
	requireRPCError(t, rpcErr, -32000, "named Jerboa network")

	// Another VM took the name while this one was stopped.
	_, rpcErr = e.call("VM.Remove", api.IDParams{ID: bare.ID})
	require.Nil(t, rpcErr)
	other, err := e.mgr.Create(context.Background(), vm.Config{Name: "web", NetworkName: faultNet, IPAddress: "10.100.0.99"})
	require.NoError(t, err)
	require.NoError(t, e.mgr.Start(context.Background(), other.ID))
	_, rpcErr = e.call("VM.SnapshotRestore", api.SnapshotParams{ID: info.ID, Name: "s"})
	requireRPCError(t, rpcErr, -32000, "held by live VM")

	// The network was removed and recreated on another subnet.
	require.NoError(t, e.mgr.Stop(context.Background(), other.ID))
	require.NoError(t, e.nets.Remove(faultNet))
	_, rpcErr = e.call("VM.SnapshotRestore", api.SnapshotParams{ID: info.ID, Name: "s"})
	requireRPCError(t, rpcErr, -32000, "not registered")
	_, err = e.nets.Create(faultNet, "10.77.0.0/24", "bridge")
	require.NoError(t, err)
	_, rpcErr = e.call("VM.SnapshotRestore", api.SnapshotParams{ID: info.ID, Name: "s"})
	requireRPCError(t, rpcErr, -32000, "changed subnet or gateway")
	assert.Equal(t, 1, sm.restores, "a refused restore must not reach the backend")
	require.Equal(t, []string{"10.77.0.1"}, leasesOf(t, e.netRoot, faultNet), "a refused restore reserved an address: only the new gateway may be held")
}

type failingSnapshots struct {
	*vm.MockManager
	restores int
}

func (m *failingSnapshots) SnapshotCreate(context.Context, string, string) (snapshot.Info, error) {
	return snapshot.Info{}, errInjected
}

func (m *failingSnapshots) SnapshotRestore(context.Context, string, string) error {
	m.restores++
	return errInjected
}
func (m *failingSnapshots) SnapshotList() ([]snapshot.Entry, error) { return nil, errInjected }
func (m *failingSnapshots) SnapshotInspect(string) (snapshot.Info, error) {
	return snapshot.Info{}, errInjected
}
func (m *failingSnapshots) SnapshotRemove(string) error { return errInjected }
