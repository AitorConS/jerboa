//go:build linux || (darwin && arm64)

package apiserver

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/scheduler"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/stretchr/testify/require"
)

// These tests are about what the RPC server does when a request cannot be
// carried out: the hypervisor refuses, the disk is full, the client lies,
// hangs up half-way or races another client. The rule they hold the daemon
// to: a failed request is answered with one clean RPC error and leaves no
// trace — no half-registered VM, no address that stays leased to nobody, no
// name or start claim that can never be taken again, no partial file.

const (
	faultNet     = "app"
	faultSubnet  = "10.100.0.0/24"
	faultGateway = "10.100.0.1"
	faultWait    = 10 * time.Second
	faultTick    = 5 * time.Millisecond
)

var errInjected = errors.New("injected hypervisor failure")

// faultEnv is a server over a mock VM manager and real stores in a temporary
// directory, with one network to run VMs on.
type faultEnv struct {
	t       *testing.T
	s       *Server
	mgr     *vm.MockManager
	nets    *network.Store
	netRoot string
	vols    *volume.Store
	volRoot string
	imgs    *image.Store
	imgRoot string
}

func newFaultEnv(t *testing.T) *faultEnv {
	t.Helper()
	root := t.TempDir()
	e := &faultEnv{
		t: t, mgr: vm.NewMockManager(),
		netRoot: filepath.Join(root, "networks"),
		volRoot: filepath.Join(root, "volumes"),
		imgRoot: filepath.Join(root, "images"),
	}
	var err error
	e.nets, err = network.NewStore(e.netRoot)
	require.NoError(t, err)
	e.vols, err = volume.NewStore(e.volRoot)
	require.NoError(t, err)
	e.imgs, err = image.NewStore(e.imgRoot)
	require.NoError(t, err)
	_, err = e.nets.Create(faultNet, faultSubnet, "bridge")
	require.NoError(t, err)
	e.s = &Server{mgr: e.mgr, netStore: e.nets, volStore: e.vols, imgStore: e.imgs, resolver: scheduler.NewResolver(e.mgr)}
	return e
}

func rawParams(t testing.TB, p any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	return raw
}

// run issues VM.Run for a VM on the test network.
func (e *faultEnv) run(p api.RunParams) (api.VMInfo, *api.RPCError) {
	e.t.Helper()
	if p.ImagePath == "" && p.Image == "" {
		p.ImagePath = "/images/app.img"
	}
	res, rpcErr := e.s.handleRun(context.Background(), rawParams(e.t, p))
	if rpcErr != nil {
		return api.VMInfo{}, rpcErr
	}
	info, ok := res.(api.VMInfo)
	require.True(e.t, ok, "VM.Run returned %T", res)
	return info, nil
}

// call dispatches a request that needs no connection.
func (e *faultEnv) call(method string, params any) (any, *api.RPCError) {
	e.t.Helper()
	return e.s.dispatch(context.Background(), &api.Request{JSONRPC: "2.0", ID: 1, Method: method, Params: rawParams(e.t, params)}, nil, nil)
}

// leases returns the addresses the network has handed out to VMs, read from
// the file a restarted daemon would read, without the gateway.
func (e *faultEnv) leases() []string {
	e.t.Helper()
	return leasesOf(e.t, e.netRoot, faultNet)
}

func leasesOf(t testing.TB, netRoot, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(netRoot, name, "state.json"))
	require.NoError(t, err)
	var st struct {
		AllocatedIPs    []string          `json:"allocated_ips"`
		PendingReleases map[string]string `json:"pending_releases"`
	}
	require.NoError(t, json.Unmarshal(data, &st))
	out := []string{}
	for _, ip := range st.AllocatedIPs {
		if ip != faultGateway {
			out = append(out, ip)
		}
	}
	sort.Strings(out)
	return out
}

// vmAddrs returns the addresses the registered VMs hold on the test network.
func (e *faultEnv) vmAddrs() []string {
	out := []string{}
	for _, v := range e.mgr.List() {
		if v.Cfg.NetworkName == faultNet && v.Cfg.IPAddress != "" {
			out = append(out, v.Cfg.IPAddress)
		}
	}
	sort.Strings(out)
	return out
}

// requireNoLeak checks the invariant every request must keep, whether it
// succeeded or failed: the leased addresses are exactly the ones registered
// VMs hold, and no start is left claimed.
func (e *faultEnv) requireNoLeak() {
	e.t.Helper()
	require.Equal(e.t, e.vmAddrs(), e.leases(), "leased addresses must be exactly those of the registered VMs")
	e.s.startingMu.Lock()
	defer e.s.startingMu.Unlock()
	require.Empty(e.t, e.s.starting, "a start claim outlived its request")
}

// stoppedVM registers a stopped VM holding a leased address on the network.
func (e *faultEnv) stoppedVM(name string) *vm.VM {
	e.t.Helper()
	info, rpcErr := e.run(api.RunParams{Name: name, NetworkName: faultNet})
	require.Nil(e.t, rpcErr)
	require.NoError(e.t, e.mgr.Stop(context.Background(), info.ID))
	v, err := e.mgr.Get(info.ID)
	require.NoError(e.t, err)
	return v
}

// failStart makes every boot fail the way a guest that dies at once does:
// the VM is registered, starts and is found stopped.
func (e *faultEnv) failStart() {
	var hook func(ctx context.Context, id string) error
	hook = func(ctx context.Context, id string) error {
		e.mgr.StartFn = nil
		defer func() { e.mgr.StartFn = hook }()
		if err := e.mgr.Start(ctx, id); err != nil {
			return err
		}
		if err := e.mgr.Stop(ctx, id); err != nil {
			return err
		}
		return errInjected
	}
	e.mgr.StartFn = hook
}

// writeFailed reports whether an injected failure made its write fail: the
// step must have been reached, and a failed directory sync is not an error.
func writeFailed(inj *durabletest.Injection, f durabletest.Failure) bool {
	return inj.Fired() && f.Step != durable.StepSyncDir
}

// restart serves the environment's stores again, as a restarted daemon
// does, and returns once its start-up recovery has run.
func (e *faultEnv) restart() string {
	e.t.Helper()
	addr := e.served(nil)
	_, err := dialFault(e.t, addr).DaemonVersion(context.Background())
	require.NoError(e.t, err)
	return addr
}

// isStateFile selects the writes of a network's lease file.
func isStateFile(path string) bool { return filepath.Base(path) == "state.json" }

// served starts a server on loopback TCP over the environment's stores and
// returns its address. configure runs before Serve.
func (e *faultEnv) served(configure func(*Server)) string {
	e.t.Helper()
	srv, err := NewServer(e.mgr, e.nets, "tcp://127.0.0.1:0", nil, "test", nil)
	require.NoError(e.t, err)
	srv.SetImageStore(e.imgs)
	srv.SetVolumeStore(e.vols)
	srv.authFailureDelay = 0
	if configure != nil {
		configure(srv)
	}
	e.s = srv
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	e.t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(e.t, err, "Serve must stop cleanly when its context ends")
		case <-time.After(faultWait):
			e.t.Error("Serve did not return after its context ended")
		}
	})
	return srv.listener.Addr().String()
}

func dialFault(t *testing.T, addr string) *api.Client {
	t.Helper()
	c, err := api.DialWithToken("tcp://"+addr, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// rawConn opens a bare connection so a test can put arbitrary bytes on the
// wire.
func rawConn(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(faultWait)))
	return conn
}

// capture runs a streaming handler against an in-memory connection and
// returns the single response it wrote, or nil if it wrote none.
func capture(t *testing.T, handler func(conn net.Conn)) *api.Response {
	t.Helper()
	server, client := net.Pipe()
	got := make(chan *api.Response, 1)
	go func() {
		var resp api.Response
		if err := json.NewDecoder(client).Decode(&resp); err != nil {
			got <- nil
			return
		}
		got <- &resp
	}()
	handler(server)
	require.NoError(t, server.Close())
	select {
	case resp := <-got:
		_ = client.Close()
		return resp
	case <-time.After(faultWait):
		t.Fatal("no response and no close from the handler")
		return nil
	}
}

// framed encodes payload the way a client streams it: length-prefixed frames
// of at most chunk bytes, then the zero-length terminator.
func framed(payload []byte, chunk int) []byte {
	var out bytes.Buffer
	for len(payload) > 0 {
		n := min(chunk, len(payload))
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(n)) //nolint:gosec // bounded by chunk
		out.Write(hdr[:])
		out.Write(payload[:n])
		payload = payload[n:]
	}
	out.Write([]byte{0, 0, 0, 0})
	return out.Bytes()
}

// tarOf packs files, in the order given, into a tar archive.
func tarOf(t testing.TB, files ...tarFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		typ := byte(tar.TypeReg)
		if f.typ != 0 {
			typ = f.typ
		}
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data)), Typeflag: typ, Linkname: f.link}))
		_, err := tw.Write(f.data)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

type tarFile struct {
	name string
	data []byte
	typ  byte
	link string
}

// arm64ELF is the smallest file the build accepts as a program.
func arm64ELF() []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	b[16] = 2
	b[18] = 183
	b[20] = 1
	b[52] = 64
	return b
}

// okMkfs stands in for the kernel toolchain: it writes a disk and succeeds.
func okMkfs(ctx context.Context, imgPath, _ string, manifest string) *exec.Cmd {
	if err := os.WriteFile(imgPath, []byte("fake-disk:"+manifest), 0o600); err != nil {
		return exec.CommandContext(ctx, "false")
	}
	return exec.CommandContext(ctx, "true")
}

// privateTemp points the daemon's temporary files at a directory of the
// test's own, so leftovers can be told from those of other processes.
func privateTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

// listTree returns every path under root, relative to it.
func listTree(t testing.TB, root string) []string {
	t.Helper()
	out := []string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(root, path); rel != "." {
			out = append(out, rel)
		}
		return nil
	}))
	return out
}

func requireRPCError(t testing.TB, rpcErr *api.RPCError, code int, contains string) {
	t.Helper()
	require.NotNil(t, rpcErr, "the request had to be refused")
	require.Equal(t, code, rpcErr.Code, "message: %s", rpcErr.Message)
	if contains != "" {
		require.Contains(t, rpcErr.Message, contains)
	}
	// An RPC error is shown to the user as is: it must be a sentence, not a
	// dump of the daemon's internals.
	require.NotContains(t, rpcErr.Message, "goroutine ")
	require.NotContains(t, rpcErr.Message, "runtime error")
	require.NotContains(t, rpcErr.Message, "\n", "multi-line RPC error: %q", rpcErr.Message)
}
