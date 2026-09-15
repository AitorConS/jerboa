//go:build darwin && arm64

package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/AitorConS/jerboa/internal/snapshot"
	"github.com/stretchr/testify/require"
)

func TestNativeFCSupervisorCommand(t *testing.T) {
	id := "0123456789ab"
	sock := nativeFCSocketPath(id)
	require.True(t, nativeFCSupervisorCommand([]byte("/bin/firecracker --api-sock "+sock+" --config-file /tmp/c.json\n"), id))
	require.True(t, nativeFCSupervisorCommand([]byte("/bin/firecracker --api-sock "+sock+"\n"), id), "restore supervisor has no config file")
	require.False(t, nativeFCSupervisorCommand([]byte("/bin/firecracker --api-sock "+sock+" --other"), id))
	require.False(t, nativeFCSupervisorCommand([]byte("/bin/firecracker --api-sock "+nativeFCSocketPath("ffffffffffff")), id))
	require.False(t, nativeFCSupervisorCommand([]byte("sh -c echo --api-sock "+sock+".bak"), id))
}

func TestRecoverCaptureKeepsJournalWhenResumeFails(t *testing.T) {
	m, snaps := snapshotTestManager(t, nil)
	v := stoppedSharedVM(t, m)
	v.State = StateRunning
	op := snapshot.Operation{Kind: snapshot.OperationCreate, VMID: v.ID, Snapshot: "snap", Phase: "capturing"}
	require.NoError(t, snaps.WriteOperation(op))
	listener, err := net.Listen("unix", m.vmSockPath(v.ID))
	require.NoError(t, err)
	var reject atomic.Bool
	reject.Store(true)
	var resumed atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/actions" {
			if reject.Load() {
				http.Error(w, "injected Resume failure", http.StatusConflict)
				return
			}
			resumed.Store(true)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		state := "Paused"
		if resumed.Load() {
			state = "Running"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"state": state})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	require.ErrorContains(t, m.RecoverSnapshotOperations(context.Background()), "injected Resume failure")
	ops, err := snaps.Operations()
	require.NoError(t, err)
	require.Len(t, ops, 1, "failed recovery must retain its retry record")
	require.False(t, resumed.Load())
	reject.Store(false)
	require.NoError(t, m.RecoverSnapshotOperations(context.Background()))
	require.True(t, resumed.Load())
	requireNoJournal(t, snaps)
}

type failingSnapshotStore struct {
	Store
	fail        bool
	failRunning bool
}

func (s *failingSnapshotStore) Save(v *VM) error {
	if s.fail || (s.failRunning && v.GetState() == StateRunning) {
		return errors.New("injected disk failure")
	}
	return s.Store.Save(v)
}

func TestSnapshotRestoreDoesNotCommitWhenRunningSaveFails(t *testing.T) {
	for _, failRollback := range []bool{false, true} {
		t.Run(fmt.Sprint("rollback-fails-", failRollback), func(t *testing.T) {
			s := &failingSnapshotStore{Store: NewMemoryStore(), failRunning: true}
			m, snaps := snapshotTestManager(t, s)
			v := stoppedSharedVM(t, m)
			publishVMSnapshot(t, snaps, v, "snap", v.ID, nativeGuestMAC(v.ID))
			var state atomic.Value
			state.Store("Paused")
			var process *exec.Cmd
			m.mkCmd = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				listener, err := net.Listen("unix", m.vmSockPath(v.ID))
				require.NoError(t, err)
				server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/":
						_ = json.NewEncoder(w).Encode(map[string]any{"state": state.Load()})
					case "/actions":
						var action map[string]string
						_ = json.NewDecoder(r.Body).Decode(&action)
						if action["action_type"] == "Resume" {
							state.Store("Running")
						} else {
							state.Store("Exited")
						}
						w.WriteHeader(http.StatusAccepted)
					case "/snapshot/load":
						w.WriteHeader(http.StatusAccepted)
						_, _ = w.Write([]byte(`{"operation_id":1}`))
					case "/operations/1":
						_, _ = w.Write([]byte(`{"status":"succeeded"}`))
					default:
						w.WriteHeader(http.StatusNoContent)
					}
				})}
				go func() { _ = server.Serve(listener) }()
				t.Cleanup(func() { _ = server.Close() })
				process = exec.CommandContext(ctx, "/bin/sleep", "30")
				t.Cleanup(func() {
					if process.Process != nil {
						_ = process.Process.Kill()
					}
				})
				return process
			}
			// Trigger the persistent failure only when Running is first saved.
			m.store = &runningSaveFailureStore{Store: s, failAllAfter: failRollback}
			err := m.SnapshotRestore(context.Background(), v.ID, "snap")
			require.ErrorContains(t, err, "persist running state: injected disk failure")
			require.Equal(t, StateStopped, v.GetState())
			require.NotNil(t, process.ProcessState, "launched supervisor was reaped")
			require.Zero(t, v.pid)
			require.Nil(t, v.proc)
			_, err = os.Stat(m.vmSockPath(v.ID))
			require.True(t, os.IsNotExist(err))
			ops, err := snaps.Operations()
			require.NoError(t, err)
			if failRollback {
				require.Len(t, ops, 1, "retain record until rollback can be persisted")
			} else {
				require.Empty(t, ops)
			}
		})
	}
}

type runningSaveFailureStore struct {
	Store
	failAllAfter bool
	failed       bool
}

func (s *runningSaveFailureStore) Save(v *VM) error {
	if v.GetState() == StateRunning {
		s.failed = true
	}
	if s.failed && s.failAllAfter {
		return errors.New("injected disk failure")
	}
	return s.Store.Save(v)
}

func TestRestorePersistenceFailureAndRecoveryRetry(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "vms")
	s := &failingSnapshotStore{Store: NewFileStore(storeDir)}
	m, snaps := snapshotTestManager(t, s)
	v := stoppedSharedVM(t, m)
	require.NoError(t, v.beginRestore())
	require.NoError(t, s.Save(v))
	require.NoError(t, snaps.WriteOperation(snapshot.Operation{Kind: snapshot.OperationRestore, VMID: v.ID, Snapshot: "snap", Phase: "resuming"}))
	s.fail = true
	require.ErrorContains(t, m.persistRestoreRunning(v), "persist running state: injected disk failure")
	require.ErrorContains(t, m.rollbackRestore(v, nil, m.vmSockPath(v.ID), nil, ""), "injected disk failure")
	require.Equal(t, StateStopped, v.GetState())
	// Recovery may retry in the same daemon after the in-memory rollback, or
	// after restart from the last durable Restoring state. Both must fail closed.
	require.ErrorContains(t, m.RecoverSnapshotOperations(context.Background()), "injected disk failure")
	ops, err := snaps.Operations()
	require.NoError(t, err)
	require.Len(t, ops, 1)
	restored := NewFileStore(storeDir)
	require.NoError(t, restored.Restore())
	s.Store = restored
	require.ErrorContains(t, m.RecoverSnapshotOperations(context.Background()), "injected disk failure")
	ops, err = snaps.Operations()
	require.NoError(t, err)
	require.Len(t, ops, 1)
	s.fail = false
	require.NoError(t, m.RecoverSnapshotOperations(context.Background()))
	requireNoJournal(t, snaps)
	final := NewFileStore(storeDir)
	require.NoError(t, final.Restore())
	persisted, err := final.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, persisted.GetState())
}

func TestSnapshotEligible(t *testing.T) {
	require.ErrorIs(t, snapshotEligible(Config{}), ErrSnapshotUnsupported)
	require.ErrorIs(t, snapshotEligible(Config{NetworkName: "n", Volumes: []VolumeMount{{DiskPath: "/d"}}}), ErrSnapshotUnsupported)
	require.NoError(t, snapshotEligible(Config{NetworkName: "n"}))
}

// shortTemp keeps Unix socket paths below the macOS 104-byte limit.
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "jbsnap")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
	return dir
}

func snapshotTestManager(t *testing.T, store Store) (*FirecrackerManager, *snapshot.Store) {
	t.Helper()
	root := shortTemp(t)
	snaps, err := snapshot.Open(filepath.Join(root, "snapshots"), snapshot.Limits{})
	require.NoError(t, err)
	opts := []FCOption{WithFCSnapshotStore(snaps)}
	if store != nil {
		opts = append(opts, WithFCStore(store))
	}
	return NewFirecrackerManager(filepath.Join(root, "missing-firecracker"), filepath.Join(root, "kernel"), opts...), snaps
}

func stoppedSharedVM(t *testing.T, m *FirecrackerManager) *VM {
	t.Helper()
	v, err := m.store.Create(Config{Memory: "256M", CPUs: 1, ImagePath: "/img", NetworkName: "shared", IPAddress: "172.30.0.5", GatewayIP: "172.30.0.1", SubnetMask: "24"})
	require.NoError(t, err)
	v.State = StateStopped
	close(v.done)
	return v
}

func writeVMArtifact(t *testing.T, dir, mac string) {
	t.Helper()
	require.NoError(t, os.Mkdir(dir, 0o700))
	data := bytes.Repeat([]byte{1}, 1024)
	sum := sha256.Sum256(data)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ram.bin"), data, 0o600))
	manifest, err := json.Marshal(map[string]any{
		"format_version": 1,
		"compatibility":  map[string]any{"host": "test"},
		"config": map[string]any{
			"machine-config":     map[string]any{"vcpu_count": 1, "mem_size_mib": 256},
			"drives":             []any{map[string]any{"drive_id": "rootfs"}},
			"network-interfaces": []any{map[string]any{"iface_id": "eth0", "guest_mac": mac, "backend": "unix-stream"}},
		},
		"components": map[string]any{"ram.bin": map[string]any{"size": len(data), "sha256": hex.EncodeToString(sum[:])}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600))
}

func publishVMSnapshot(t *testing.T, snaps *snapshot.Store, v *VM, name, vmID, mac string) {
	t.Helper()
	res, err := snaps.Reserve(name, 1)
	require.NoError(t, err)
	writeVMArtifact(t, res.ArtifactPath(), mac)
	subnet, err := snapshotSubnet(v.Cfg)
	require.NoError(t, err)
	_, err = res.Publish(snapshot.Info{
		VMID: vmID, Backend: snapshotBackend, Memory: v.Cfg.Memory, CPUs: 1, ConfigSHA256: snapshotConfigDigest(v),
		Network: snapshot.NetworkInfo{Name: v.Cfg.NetworkName, Subnet: subnet, Gateway: v.Cfg.GatewayIP, IP: v.Cfg.IPAddress, MAC: mac},
	}, nil)
	require.NoError(t, err)
}

func requireNoJournal(t *testing.T, snaps *snapshot.Store) {
	t.Helper()
	ops, err := snaps.Operations()
	require.NoError(t, err)
	require.Empty(t, ops)
}

func TestSnapshotRestoreRejectsForeignSnapshots(t *testing.T) {
	m, snaps := snapshotTestManager(t, nil)
	v := stoppedSharedVM(t, m)
	publishVMSnapshot(t, snaps, v, "other-vm", "ffffffffffff", nativeGuestMAC(v.ID))
	publishVMSnapshot(t, snaps, v, "wrong-mac", v.ID, "02:00:00:00:00:fe")

	err := m.SnapshotRestore(context.Background(), v.ID, "other-vm")
	require.ErrorContains(t, err, "belongs to VM ffffffffffff")
	err = m.SnapshotRestore(context.Background(), v.ID, "wrong-mac")
	require.ErrorContains(t, err, "MAC")
	err = m.SnapshotRestore(context.Background(), v.ID, "missing")
	require.ErrorIs(t, err, snapshot.ErrNotFound)
	require.Equal(t, StateStopped, v.GetState())
	requireNoJournal(t, snaps)
}

func TestSnapshotRestoreRejectsChangedConfig(t *testing.T) {
	m, snaps := snapshotTestManager(t, nil)
	v := stoppedSharedVM(t, m)
	publishVMSnapshot(t, snaps, v, "snap", v.ID, nativeGuestMAC(v.ID))
	v.Cfg.NetworkAliases = []string{"new-alias"}
	require.ErrorContains(t, m.SnapshotRestore(context.Background(), v.ID, "snap"), "configuration differs")
	requireNoJournal(t, snaps)
}

func TestSnapshotRestoreRollsBackLaunchFailure(t *testing.T) {
	m, snaps := snapshotTestManager(t, nil)
	v := stoppedSharedVM(t, m)
	publishVMSnapshot(t, snaps, v, "snap", v.ID, nativeGuestMAC(v.ID))

	err := m.SnapshotRestore(context.Background(), v.ID, "snap")
	require.ErrorContains(t, err, "launch supervisor")
	require.Equal(t, StateStopped, v.GetState())
	<-v.Done()
	requireNoJournal(t, snaps)
	m.nativeMu.Lock()
	require.Empty(t, m.nativeState, "network link and publications released")
	m.nativeMu.Unlock()
	_, statErr := os.Stat(v.Cfg.nativeSocket)
	require.True(t, os.IsNotExist(statErr), "switch socket removed")
	require.NoError(t, snaps.Remove("snap"), "snapshot no longer in use")

	v.State = StateRunning
	require.ErrorContains(t, m.SnapshotRestore(context.Background(), v.ID, "missing"), "running")
}

func TestSnapshotRestoreRefusesPendingRestart(t *testing.T) {
	m, snaps := snapshotTestManager(t, nil)
	v := stoppedSharedVM(t, m)
	publishVMSnapshot(t, snaps, v, "snap", v.ID, nativeGuestMAC(v.ID))
	m.claims.setRestartPending(v.ID, true)
	require.ErrorIs(t, m.SnapshotRestore(context.Background(), v.ID, "snap"), ErrVMBusy)
}

func TestSnapshotCreateRequiresRunningSharedVM(t *testing.T) {
	m, snaps := snapshotTestManager(t, nil)
	v := stoppedSharedVM(t, m)
	_, err := m.SnapshotCreate(context.Background(), v.ID, "snap")
	require.ErrorContains(t, err, "running VM is required")
	_, err = m.SnapshotCreate(context.Background(), v.ID, "../escape")
	require.ErrorIs(t, err, snapshot.ErrInvalidName)
	_, err = snaps.Reserve("snap", 1)
	require.NoError(t, err, "a rejected create leaves no reservation")
}

func TestSnapshotUnsupportedWithoutStore(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "kernel")
	_, err := m.SnapshotCreate(context.Background(), "x", "snap")
	require.ErrorIs(t, err, ErrSnapshotUnsupported)
	require.ErrorIs(t, m.SnapshotRestore(context.Background(), "x", "snap"), ErrSnapshotUnsupported)
	require.NoError(t, m.RecoverSnapshotOperations(context.Background()))
}

func TestRecoverSnapshotOperationsRollsBackInterruptedRestore(t *testing.T) {
	root := t.TempDir()
	fs := NewFileStore(filepath.Join(root, "vms"))
	m, snaps := snapshotTestManager(t, fs)
	v, err := fs.Create(Config{Memory: "256M", NetworkName: "shared", IPAddress: "172.30.0.5", GatewayIP: "172.30.0.1"})
	require.NoError(t, err)
	orphan, err := fs.Create(Config{Memory: "256M", NetworkName: "shared", IPAddress: "172.30.0.6", GatewayIP: "172.30.0.1"})
	require.NoError(t, err)
	for _, x := range []*VM{v, orphan} {
		x.State = StateRestoring
		require.NoError(t, fs.Save(x))
	}
	// The PID is not a supervisor for this VM, so recovery must not signal it.
	require.NoError(t, snaps.WriteOperation(snapshot.Operation{Kind: snapshot.OperationRestore, VMID: v.ID, Snapshot: "snap", Phase: "loading", PID: os.Getpid()}))
	res, err := snaps.Reserve("partial", 1)
	require.NoError(t, err)
	_ = res
	staleLink := filepath.Join(qmpRuntimeDir(), "net-"+v.ID+".sock")
	require.NoError(t, os.WriteFile(staleLink, nil, 0o600))

	restarted := NewFileStore(filepath.Join(root, "vms"))
	require.NoError(t, restarted.Restore())
	m.store = restarted
	require.NoError(t, m.RecoverSnapshotOperations(context.Background()))

	for _, id := range []string{v.ID, orphan.ID} {
		got, err := restarted.Get(id)
		require.NoError(t, err)
		require.Equal(t, StateStopped, got.GetState())
		require.True(t, got.DaemonRecovered)
		<-got.Done()
	}
	requireNoJournal(t, snaps)
	_, err = os.Stat(staleLink)
	require.True(t, os.IsNotExist(err), "crashed daemon's switch socket removed")
	entries, err := os.ReadDir(filepath.Join(snaps.Root(), ".staging"))
	require.NoError(t, err)
	require.Empty(t, entries)

	persisted := NewFileStore(filepath.Join(root, "vms"))
	require.NoError(t, persisted.Restore())
	got, err := persisted.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, got.GetState(), "rollback is durable")
}
