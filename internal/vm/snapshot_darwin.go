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
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AitorConS/jerboa/internal/snapshot"
)

// Snapshot v1 is deliberately narrow: in-place create/restore of a
// Firecracker/HVF VM attached to a named Jerboa network (unix-stream), with no
// Jerboa volumes. The fork captures RAM, vCPU/GIC, VirtIO state and the
// ephemeral root disk; the daemon recreates the switch link, service DNS,
// aliases and publications from the VM's own configuration and current policy.
const (
	snapshotBackend        = "firecracker-hvf"
	snapshotCaptureTimeout = 10 * time.Minute
	snapshotLoadTimeout    = 10 * time.Minute
	snapshotStateTimeout   = 35 * time.Second
	snapshotTermGrace      = 5 * time.Second
	snapshotRecoverWait    = 2 * time.Minute
)

// SnapshotCreate pauses a running VM, captures it into the private store and
// resumes it. The guest is paused for the whole capture.
func (m *FirecrackerManager) SnapshotCreate(ctx context.Context, id, name string) (info snapshot.Info, err error) {
	if m.snapshots == nil {
		return info, fmt.Errorf("%w: the snapshot store is not configured", ErrSnapshotUnsupported)
	}
	if err := snapshot.ValidateName(name); err != nil {
		return info, err
	}
	v, releaseVM, err := m.claimVM(id, opSnapshotCreate)
	if err != nil {
		return info, fmt.Errorf("snapshot create: %w", err)
	}
	defer releaseVM()
	if st := v.GetState(); st != StateRunning {
		return info, fmt.Errorf("snapshot create %s: vm is %s; a running VM is required", v.ID, st)
	}
	if err := snapshotEligible(v.Cfg); err != nil {
		return info, fmt.Errorf("snapshot create %s: %w", v.ID, err)
	}
	mac := v.Cfg.nativeMAC
	if mac == "" || mac != nativeGuestMAC(v.ID) {
		return info, fmt.Errorf("snapshot create %s: shared network link is not prepared", v.ID)
	}
	socket := m.vmSockPath(v.ID)
	if s, err := readNativeFCState(ctx, socket); err != nil || s.State != "Running" {
		return info, fmt.Errorf("snapshot create %s: guest is not Running (state %q, %v)", v.ID, s.State, err)
	}
	mem, err := parseMiB(v.Cfg.Memory)
	if err != nil {
		return info, fmt.Errorf("snapshot create %s: %w", v.ID, err)
	}
	estimate := int64(mem)<<20 + fileSize(m.rootfsPath(v.ID)) + fileSize(m.kernelImage) + 1<<20
	res, err := m.snapshots.Reserve(name, estimate)
	if err != nil {
		return info, fmt.Errorf("snapshot create %s: %w", v.ID, err)
	}
	defer func() {
		if err != nil {
			res.Abort()
		}
	}()
	journal := func(phase string) error {
		return m.snapshots.WriteOperation(snapshot.Operation{Kind: snapshot.OperationCreate, VMID: v.ID, Snapshot: name, Phase: phase})
	}
	if err = journal("pausing"); err != nil {
		return info, err
	}
	// Even a failed HTTP response may have delivered Pause. Settle the guest
	// before deleting its recovery record, including ambiguous request failures.
	defer func() {
		if rerr := m.resumeAfterSnapshot(v, socket); rerr != nil {
			if err != nil {
				err = fmt.Errorf("%w; %v", err, rerr)
			} else {
				err = fmt.Errorf("snapshot %s was published, but %w", name, rerr)
			}
			return
		}
		if rerr := m.snapshots.RemoveOperation(v.ID); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	if _, err = nativeFCAccepted(ctx, socket, http.MethodPut, "/actions", map[string]string{"action_type": "Pause"}); err != nil {
		return info, fmt.Errorf("snapshot create %s: %w", v.ID, err)
	}
	if err = waitNativeFCState(ctx, socket, snapshotStateTimeout, "Paused"); err != nil {
		return info, fmt.Errorf("snapshot create %s: pause: %w", v.ID, err)
	}
	if err = journal("capturing"); err != nil {
		return info, err
	}
	var op uint64
	if op, err = nativeFCAccepted(ctx, socket, http.MethodPut, "/snapshot/create", map[string]string{"snapshot_path": res.ArtifactPath()}); err != nil {
		return info, fmt.Errorf("snapshot create %s: %w", v.ID, err)
	}
	if err = waitNativeFCOperation(ctx, socket, op, snapshotCaptureTimeout); err != nil {
		return info, fmt.Errorf("snapshot create %s: capture: %w", v.ID, err)
	}
	if err = journal("publishing"); err != nil {
		return info, err
	}
	subnet, err := snapshotSubnet(v.Cfg)
	if err != nil {
		return info, fmt.Errorf("snapshot create %s: %w", v.ID, err)
	}
	info = snapshot.Info{
		CreatedAt:    time.Now().UTC(),
		VMID:         v.ID,
		VMName:       v.Cfg.Name,
		Backend:      snapshotBackend,
		Image:        v.Cfg.ImageRef,
		ImageDigest:  v.Cfg.ImageDigest,
		Memory:       v.Cfg.Memory,
		CPUs:         max(v.Cfg.CPUs, 1),
		ConfigSHA256: snapshotConfigDigest(v),
		Network: snapshot.NetworkInfo{
			Name:    v.Cfg.NetworkName,
			Subnet:  subnet,
			Gateway: v.Cfg.GatewayIP,
			IP:      v.Cfg.IPAddress,
			MAC:     mac,
			Aliases: append([]string(nil), v.Cfg.NetworkAliases...),
		},
		Ports: snapshotPorts(v.Cfg.PortMaps),
	}
	info, err = res.Publish(info, func(a snapshot.Artifact) error { return checkSnapshotManifest(a, v.Cfg, mac) })
	if err != nil {
		return snapshot.Info{}, fmt.Errorf("snapshot create %s: %w", v.ID, err)
	}
	slog.Info("snapshot created", "vm_id", v.ID, "snapshot", name, "bytes", info.SizeBytes)
	return info, nil
}

// resumeAfterSnapshot always tries to return the guest to Running. If that is
// impossible the VM is force-stopped so Jerboa never reports a paused guest as
// running.
func (m *FirecrackerManager) resumeAfterSnapshot(v *VM, socket string) error {
	ctx, cancel := context.WithTimeout(context.Background(), snapshotCaptureTimeout)
	defer cancel()
	err := func() error {
		s, err := readNativeFCState(ctx, socket)
		if err != nil {
			return err
		}
		switch s.State {
		case "Running":
			return nil
		case "Paused":
		default:
			// A capture cancelled by our own timeout still settles to Paused.
			if err := waitNativeFCState(ctx, socket, snapshotCaptureTimeout, "Paused", "Running"); err != nil {
				return err
			}
			if s, err = readNativeFCState(ctx, socket); err == nil && s.State == "Running" {
				return nil
			}
		}
		if _, err := nativeFCAccepted(ctx, socket, http.MethodPut, "/actions", map[string]string{"action_type": "Resume"}); err != nil {
			return err
		}
		return waitNativeFCState(ctx, socket, snapshotStateTimeout, "Running")
	}()
	if err == nil {
		return nil
	}
	if st := v.GetState(); st != StateRunning {
		return fmt.Errorf("resume after snapshot failed: %v (VM is %s)", err, st)
	}
	v.AddWarning("snapshot resume failed; the VM was force-stopped instead of reporting a paused guest as running")
	if terr := v.transition(StateStopping); terr == nil {
		_ = m.store.Save(v)
	}
	m.hchecker.Stop(v.ID)
	v.SetExplicitStop()
	v.mu.RLock()
	proc := v.proc
	v.mu.RUnlock()
	if proc != nil {
		_ = proc.kill()
	}
	return fmt.Errorf("resume after snapshot failed: %v; VM %s was force-stopped so a paused guest is not reported as running", err, v.ID)
}

// SnapshotRestore restores a stopped VM in place. The VM keeps its ID, MAC,
// IP, name, aliases and published ports; the network policy is the daemon's
// current one. Any failure rolls back to stopped; a daemon crash is finished
// by RecoverSnapshotOperations.
func (m *FirecrackerManager) SnapshotRestore(ctx context.Context, id, name string) (err error) {
	if m.snapshots == nil {
		return fmt.Errorf("%w: the snapshot store is not configured", ErrSnapshotUnsupported)
	}
	if err := snapshot.ValidateName(name); err != nil {
		return err
	}
	v, releaseVM, err := m.claimVM(id, opSnapshotRestore)
	if err != nil {
		return fmt.Errorf("snapshot restore: %w", err)
	}
	defer releaseVM()
	if m.claims.restartPending(v.ID) {
		return fmt.Errorf("snapshot restore %s: %w: automatic restart pending", v.ID, ErrVMBusy)
	}
	if st := v.GetState(); st != StateStopped {
		return fmt.Errorf("snapshot restore %s: vm is %s; restore is in place and requires a stopped VM", v.ID, st)
	}
	if err := snapshotEligible(v.Cfg); err != nil {
		return fmt.Errorf("snapshot restore %s: %w", v.ID, err)
	}
	release, err := m.snapshots.Acquire(name)
	if err != nil {
		return fmt.Errorf("snapshot restore %s: %w", v.ID, err)
	}
	defer release()
	info, artifact, artifactPath, err := m.snapshots.Verify(name)
	if err != nil {
		return fmt.Errorf("snapshot restore %s: %w", v.ID, err)
	}
	if err := validateRestoreTarget(v, info, artifact); err != nil {
		return fmt.Errorf("snapshot restore %s: %w", v.ID, err)
	}

	pid := 0
	journal := func(phase string) error {
		return m.snapshots.WriteOperation(snapshot.Operation{Kind: snapshot.OperationRestore, VMID: v.ID, Snapshot: name, Phase: phase, PID: pid})
	}
	if err := journal("validated"); err != nil {
		return err
	}
	if err := v.beginRestore(); err != nil {
		_ = m.snapshots.RemoveOperation(v.ID)
		return fmt.Errorf("snapshot restore %s: %w", v.ID, err)
	}
	var (
		cmd       *exec.Cmd
		cleanup   func()
		workDir   string
		committed bool
		socket    = m.vmSockPath(v.ID)
	)
	defer func() {
		if committed {
			return
		}
		if rerr := m.rollbackRestore(v, cmd, socket, cleanup, workDir); rerr != nil {
			err = errors.Join(err, rerr)
			return // Keep the journal until rollback is durable.
		}
		if rerr := m.snapshots.RemoveOperation(v.ID); rerr != nil {
			slog.Warn("snapshot restore: remove journal after rollback", "vm_id", v.ID, "err", rerr)
		}
		slog.Warn("snapshot restore rolled back", "vm_id", v.ID, "snapshot", name, "err", err)
	}()
	fail := func(format string, args ...any) error {
		return fmt.Errorf("snapshot restore %s: "+format, append([]any{v.ID}, args...)...)
	}
	if err = m.store.Save(v); err != nil {
		return fail("persist restoring state: %w", err)
	}
	if cleanup, err = m.prepareFCHost(v); err != nil {
		return fail("host network: %w", err)
	}
	if v.Cfg.nativeMAC != info.Network.MAC {
		return fail("prepared MAC %s does not match snapshot MAC %s", v.Cfg.nativeMAC, info.Network.MAC)
	}
	if err = journal("host-prepared"); err != nil {
		return err
	}
	netCfg := []nativeFCNetwork{{IfaceID: "eth0", Backend: "unix-stream", GuestMAC: v.Cfg.nativeMAC, SocketPath: v.Cfg.nativeSocket}}
	security := map[string]any{"version": 1, "unix_stream": v.Cfg.nativeSocket}
	if workDir, err = os.MkdirTemp("", "jerboa-fc-"+v.ID+"-"); err != nil {
		return fail("%w", err)
	}
	record, _ := json.MarshalIndent(map[string]any{"snapshot": name, "network-interfaces": netCfg, "security": security}, "", "  ")
	if err = os.WriteFile(filepath.Join(workDir, "config.json"), record, 0o600); err != nil {
		return fail("%w", err)
	}
	_ = os.Remove(socket)
	cmd = m.mkCmd(ctx, m.fcBin, "--api-sock", socket)
	cmd.Stdout = &v.logBuf
	cmd.Stderr = &v.logBuf
	if err = cmd.Start(); err != nil {
		cmd = nil
		return fail("launch supervisor: %w", err)
	}
	pid = cmd.Process.Pid
	v.mu.Lock()
	v.proc = &osProcess{cmd.Process}
	v.pid = pid
	v.mu.Unlock()
	if err = journal("supervisor-started"); err != nil {
		return err
	}
	_ = m.store.Save(v)
	if err = awaitFCReachable(ctx, socket); err != nil {
		return fail("%w", err)
	}
	if err = nativeFCConfigure(ctx, socket, "/network-interfaces", netCfg); err != nil {
		return fail("%w", err)
	}
	if err = nativeFCConfigure(ctx, socket, "/security", security); err != nil {
		return fail("%w", err)
	}
	if err = journal("loading"); err != nil {
		return err
	}
	var op uint64
	if op, err = nativeFCAccepted(ctx, socket, http.MethodPut, "/snapshot/load", map[string]string{"snapshot_path": artifactPath}); err != nil {
		return fail("%w", err)
	}
	if err = waitNativeFCOperation(ctx, socket, op, snapshotLoadTimeout); err != nil {
		return fail("load: %w", err)
	}
	if err = waitNativeFCState(ctx, socket, snapshotStateTimeout, "Paused"); err != nil {
		return fail("load: %w", err)
	}
	if err = journal("resuming"); err != nil {
		return err
	}
	if _, err = nativeFCAccepted(ctx, socket, http.MethodPut, "/actions", map[string]string{"action_type": "Resume"}); err != nil {
		return fail("%w", err)
	}
	if err = waitNativeFCState(ctx, socket, snapshotStateTimeout, "Running"); err != nil {
		return fail("resume: %w", err)
	}
	now := time.Now()
	v.mu.Lock()
	v.StartedAt = &now
	v.StoppedAt = nil
	v.mu.Unlock()
	if newStatsCollector != nil {
		v.SetStatsProvider(newStatsCollector(pid, v).Collect)
	}
	if err = m.persistRestoreRunning(v); err != nil {
		return fail("%w", err)
	}
	committed = true
	if rerr := m.snapshots.RemoveOperation(v.ID); rerr != nil {
		slog.Warn("snapshot restore: remove journal", "vm_id", v.ID, "err", rerr)
	}
	prepareNativeFCHealth(v)
	m.hchecker.Start(ctx, v)
	go m.monitor(v, cmd, socket, filepath.Join(workDir, "config.json"), m.vmmLogPath(v.ID), m.rootfsPath(v.ID))
	slog.Info("snapshot restored", "vm_id", v.ID, "snapshot", name, "pid", pid)
	return nil
}

func (m *FirecrackerManager) persistRestoreRunning(v *VM) error {
	if err := v.transition(StateRunning); err != nil {
		return err
	}
	if err := m.store.Save(v); err != nil {
		return fmt.Errorf("persist running state: %w", err)
	}
	return nil
}

// rollbackRestore stops only the supervisor this restore launched, closes the
// link and publications and returns the VM to stopped.
func (m *FirecrackerManager) rollbackRestore(v *VM, cmd *exec.Cmd, socket string, cleanup func(), workDir string) error {
	if cmd != nil && cmd.Process != nil {
		stopNativeGuest(socket)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(snapshotTermGrace):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	if cleanup != nil {
		cleanup()
	}
	now := time.Now()
	v.mu.Lock()
	v.hostCleanup = nil
	v.proc = nil
	v.pid = 0
	v.StoppedAt = &now
	v.mu.Unlock()
	_ = os.Remove(socket)
	if workDir != "" {
		_ = os.RemoveAll(workDir)
	}
	// The guest may already have reached Running before persistence failed.
	v.mu.Lock()
	v.State = StateStopped
	select {
	case <-v.done:
	default:
		close(v.done)
	}
	v.mu.Unlock()
	if err := m.store.Save(v); err != nil {
		return fmt.Errorf("snapshot restore rollback: persist: %w", err)
	}
	return nil
}

// RecoverSnapshotOperations finishes lifecycles interrupted by a daemon crash.
// Call it after the VM store is restored and before RestoreHostRuntime: a
// restore whose guest is already Running is adopted, every other restore is
// rolled back to stopped, and a paused capture is resumed. Incomplete staging
// data is removed last.
func (m *FirecrackerManager) RecoverSnapshotOperations(ctx context.Context) error {
	if m.snapshots == nil {
		return nil
	}
	ops, err := m.snapshots.Operations()
	if err != nil {
		return fmt.Errorf("snapshot recovery: %w", err)
	}
	handled := map[string]bool{}
	for _, op := range ops {
		handled[op.VMID] = true
		slog.Info("snapshot recovery: interrupted operation", "kind", op.Kind, "vm_id", op.VMID, "snapshot", op.Snapshot, "phase", op.Phase)
		switch op.Kind {
		case snapshot.OperationRestore:
			err = m.recoverRestore(ctx, op)
		case snapshot.OperationCreate:
			err = m.recoverCreate(ctx, op)
		default:
			err = fmt.Errorf("unknown operation kind %q", op.Kind)
		}
		if err != nil {
			return fmt.Errorf("snapshot recovery %s: %w", op.VMID, err)
		}
		if err := m.snapshots.RemoveOperation(op.VMID); err != nil {
			return fmt.Errorf("snapshot recovery: %w", err)
		}
	}
	for _, v := range m.List() {
		if v.GetState() == StateRestoring && !handled[v.ID] {
			if err := m.abandonRestore(v); err != nil {
				return err
			}
		}
	}
	removed, err := m.snapshots.RecoverStaging()
	if len(removed) > 0 {
		slog.Info("snapshot recovery: removed incomplete staging entries", "entries", removed)
	}
	return err
}

func (m *FirecrackerManager) recoverRestore(ctx context.Context, op snapshot.Operation) error {
	socket := m.vmSockPath(op.VMID)
	owned := op.PID > 0 && processAlive(op.PID) && processIsNativeFCSupervisor(op.PID, op.VMID)
	v, err := m.store.Get(op.VMID)
	if err != nil {
		if owned {
			terminateNativeSupervisor(op.PID, socket)
		}
		return nil
	}
	switch v.GetState() {
	case StateRunning:
		// Committed and persisted before the journal was removed; the store
		// already re-adopted (or marked stopped) the supervisor by PID.
		return m.store.Save(v)
	case StateRestoring:
		if owned {
			if s, err := readNativeFCState(ctx, socket); err == nil && s.State == "Running" {
				p, _ := os.FindProcess(op.PID)
				now := time.Now()
				v.mu.Lock()
				v.State = StateRunning
				v.pid = op.PID
				v.proc = &osProcess{p}
				if v.StartedAt == nil {
					v.StartedAt = &now
				}
				v.mu.Unlock()
				if err := m.store.Save(v); err != nil {
					return err
				}
				go adoptMonitor(m.store, v)
				slog.Info("snapshot recovery: adopted restored guest", "vm_id", v.ID, "pid", op.PID)
				return nil
			}
			terminateNativeSupervisor(op.PID, socket)
		}
		return m.abandonRestore(v)
	default:
		if owned {
			terminateNativeSupervisor(op.PID, socket)
		}
	}
	return m.store.Save(v)
}

func (m *FirecrackerManager) abandonRestore(v *VM) error {
	now := time.Now()
	v.mu.Lock()
	if v.State != StateRestoring {
		v.mu.Unlock()
		return nil
	}
	v.State = StateStopped
	v.StoppedAt = &now
	v.DaemonRecovered = true
	v.pid = 0
	v.proc = nil
	close(v.done)
	v.mu.Unlock()
	// The crashed daemon could not unlink its switch listener; the API socket
	// belonged to the supervisor recovery just stopped.
	_ = os.Remove(m.vmSockPath(v.ID))
	_ = os.Remove(filepath.Join(qmpRuntimeDir(), "net-"+v.ID+".sock"))
	v.AddWarning("snapshot restore was interrupted by a daemon restart and rolled back; the VM is stopped")
	if err := m.store.Save(v); err != nil {
		return fmt.Errorf("snapshot recovery: persist rolled-back vm: %w", err)
	}
	slog.Info("snapshot recovery: rolled back interrupted restore", "vm_id", v.ID)
	return nil
}

func (m *FirecrackerManager) recoverCreate(ctx context.Context, op snapshot.Operation) error {
	v, err := m.store.Get(op.VMID)
	if err != nil || v.GetState() != StateRunning {
		return nil
	}
	socket := m.vmSockPath(v.ID)
	wait, cancel := context.WithTimeout(ctx, snapshotRecoverWait)
	defer cancel()
	if err := waitNativeFCState(wait, socket, snapshotRecoverWait, "Paused", "Running"); err != nil {
		return fmt.Errorf("confirm interrupted capture state: %w", err)
	}
	s, err := readNativeFCState(ctx, socket)
	if err != nil {
		return fmt.Errorf("read interrupted capture state: %w", err)
	}
	if s.State == "Paused" {
		if _, err = nativeFCAccepted(ctx, socket, http.MethodPut, "/actions", map[string]string{"action_type": "Resume"}); err == nil {
			err = waitNativeFCState(wait, socket, snapshotStateTimeout, "Running")
		}
		if err != nil {
			return fmt.Errorf("resume interrupted capture: %w", err)
		}
		slog.Info("snapshot recovery: resumed guest paused by interrupted capture", "vm_id", v.ID)
	} else if s.State != "Running" {
		return fmt.Errorf("interrupted capture is still %s", s.State)
	}
	return nil
}

// SnapshotList returns every store entry, including invalid ones.
func (m *FirecrackerManager) SnapshotList() ([]snapshot.Entry, error) {
	if m.snapshots == nil {
		return nil, fmt.Errorf("%w: the snapshot store is not configured", ErrSnapshotUnsupported)
	}
	return m.snapshots.List()
}

// SnapshotInspect reads a sidecar without hashing components.
func (m *FirecrackerManager) SnapshotInspect(name string) (snapshot.Info, error) {
	if m.snapshots == nil {
		return snapshot.Info{}, fmt.Errorf("%w: the snapshot store is not configured", ErrSnapshotUnsupported)
	}
	return m.snapshots.Get(name)
}

// SnapshotRemove deletes an entry not used by a restore.
func (m *FirecrackerManager) SnapshotRemove(name string) error {
	if m.snapshots == nil {
		return fmt.Errorf("%w: the snapshot store is not configured", ErrSnapshotUnsupported)
	}
	return m.snapshots.Remove(name)
}

func snapshotEligible(c Config) error {
	switch {
	case c.NetworkName == "":
		return fmt.Errorf("%w: v1 snapshots require a VM attached to a named Jerboa network", ErrSnapshotUnsupported)
	case len(c.Volumes) > 0:
		return fmt.Errorf("%w: v1 snapshots reject VMs with Jerboa volumes because volume consistency cannot be guaranteed", ErrSnapshotUnsupported)
	case c.EmulateX86:
		return fmt.Errorf("%w: x86 emulation is not a Firecracker/HVF configuration", ErrSnapshotUnsupported)
	}
	return nil
}

func validateRestoreTarget(v *VM, info snapshot.Info, a snapshot.Artifact) error {
	c := v.Cfg
	if info.VMID != v.ID {
		return fmt.Errorf("snapshot %s belongs to VM %s; v1 restores only into the same VM", info.Name, info.VMID)
	}
	if info.Backend != snapshotBackend {
		return fmt.Errorf("snapshot backend %q is not %s", info.Backend, snapshotBackend)
	}
	if info.ConfigSHA256 != snapshotConfigDigest(v) {
		return fmt.Errorf("VM configuration differs from the snapshot")
	}
	subnet, err := snapshotSubnet(c)
	if err != nil {
		return err
	}
	n := info.Network
	if n.Name != c.NetworkName || n.IP != c.IPAddress || n.Gateway != c.GatewayIP || n.Subnet != subnet ||
		!slices.Equal(n.Aliases, nonNil(c.NetworkAliases)) && !(len(n.Aliases) == 0 && len(c.NetworkAliases) == 0) {
		return fmt.Errorf("snapshot network identity does not match the VM")
	}
	if !slices.Equal(info.Ports, snapshotPorts(c.PortMaps)) && !(len(info.Ports) == 0 && len(c.PortMaps) == 0) {
		return fmt.Errorf("snapshot published ports do not match the VM")
	}
	mac := nativeGuestMAC(v.ID)
	if n.MAC != mac {
		return fmt.Errorf("snapshot MAC %s does not match the VM MAC %s", n.MAC, mac)
	}
	return checkSnapshotManifest(a, c, mac)
}

// checkSnapshotManifest ties the fork artifact to the VM: one unix-stream eth0
// with the exact MAC, only the root disk, and the VM's CPU/memory shape.
func checkSnapshotManifest(a snapshot.Artifact, c Config, mac string) error {
	nets := a.Config.Networks
	if len(nets) != 1 || nets[0].Backend != "unix-stream" || nets[0].IfaceID != "eth0" || !strings.EqualFold(nets[0].GuestMAC, mac) {
		return fmt.Errorf("snapshot network device is not the VM's unix-stream eth0 with MAC %s", mac)
	}
	if len(a.Config.Drives) != 1 {
		return fmt.Errorf("%w: v1 snapshots contain only the ephemeral root disk", ErrSnapshotUnsupported)
	}
	mem, err := parseMiB(c.Memory)
	if err != nil {
		return err
	}
	if a.Config.Machine.CPUs != max(c.CPUs, 1) || a.Config.Machine.Memory != mem {
		return fmt.Errorf("snapshot CPU/memory shape does not match the VM")
	}
	return nil
}

// snapshotConfigDigest fingerprints the configuration a restore depends on.
// Environment values are hashed, never stored in the sidecar.
func snapshotConfigDigest(v *VM) string {
	c := v.Cfg
	env := sha256.Sum256([]byte(strings.Join(c.Env, "\x00")))
	doc := struct {
		ID, Architecture, ImagePath, ImageDigest, Memory string
		CPUs                                             int
		EmulateX86                                       bool
		Network, IP, Gateway, Mask                       string
		Aliases                                          []string
		Ports                                            []snapshot.PortInfo
		EnvSHA256                                        string
		Volumes                                          int
		DiskIOPS                                         uint64
		DiskBPS                                          int64
	}{v.ID, c.Architecture, c.ImagePath, c.ImageDigest, c.Memory, max(c.CPUs, 1), c.EmulateX86,
		c.NetworkName, c.IPAddress, c.GatewayIP, c.SubnetMask, nonNil(c.NetworkAliases), nonNil(snapshotPorts(c.PortMaps)),
		hex.EncodeToString(env[:]), len(c.Volumes), c.DiskIOPS, c.DiskBPS}
	data, _ := json.Marshal(doc)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func snapshotPorts(pms []PortMap) []snapshot.PortInfo {
	var out []snapshot.PortInfo
	for _, p := range pms {
		out = append(out, snapshot.PortInfo{HostPort: p.HostPort, GuestPort: p.GuestPort, Protocol: string(p.Protocol), BindAddr: p.BindAddr})
	}
	return out
}

func snapshotSubnet(c Config) (string, error) {
	mask := c.SubnetMask
	if mask == "" {
		mask = "24"
	}
	_, n, err := net.ParseCIDR(c.IPAddress + "/" + mask)
	if err != nil {
		return "", fmt.Errorf("invalid VM network address: %w", err)
	}
	return n.String(), nil
}

// nativeGuestMAC is the deterministic MAC prepareHostVM assigns to a VM ID.
func nativeGuestMAC(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

func fileSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func nativeFCDo(ctx context.Context, socket, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(data)
	}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, err
}

func nativeFCFault(path string, code int, data []byte) error {
	var f struct {
		Code    string `json:"fault_code"`
		Message string `json:"fault_message"`
	}
	_ = json.Unmarshal(data, &f)
	msg := f.Message
	if msg == "" {
		msg = strings.TrimSpace(string(data))
	}
	return fmt.Errorf("HVF %s: HTTP %d: %s", path, code, msg)
}

func nativeFCAccepted(ctx context.Context, socket, method, path string, body any) (uint64, error) {
	code, data, err := nativeFCDo(ctx, socket, method, path, body)
	if err != nil {
		return 0, fmt.Errorf("HVF %s: %w", path, err)
	}
	if code != http.StatusAccepted && code != http.StatusOK && code != http.StatusNoContent {
		return 0, nativeFCFault(path, code, data)
	}
	var r struct {
		ID uint64 `json:"operation_id"`
	}
	_ = json.Unmarshal(data, &r)
	return r.ID, nil
}

func nativeFCConfigure(ctx context.Context, socket, path string, body any) error {
	code, data, err := nativeFCDo(ctx, socket, http.MethodPut, path, body)
	if err != nil {
		return fmt.Errorf("HVF %s: %w", path, err)
	}
	if code != http.StatusNoContent {
		return nativeFCFault(path, code, data)
	}
	return nil
}

func waitNativeFCState(ctx context.Context, socket string, timeout time.Duration, want ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last nativeFCState
	var lastErr error
	for {
		last, lastErr = readNativeFCState(ctx, socket)
		if lastErr == nil {
			if slices.Contains(want, last.State) {
				return nil
			}
			if last.State == "Failed" || last.State == "Exited" {
				return fmt.Errorf("guest is %s: %s", last.State, last.LastError)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s (last %q, %v): %w", strings.Join(want, "/"), last.State, lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitNativeFCOperation(ctx context.Context, socket string, id uint64, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	path := "/operations/" + strconv.FormatUint(id, 10)
	for {
		code, data, err := nativeFCDo(ctx, socket, http.MethodGet, path, nil)
		if err == nil && code == http.StatusOK {
			var op struct {
				Status string          `json:"status"`
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(data, &op) == nil {
				switch op.Status {
				case "succeeded":
					return nil
				case "failed", "cancelled":
					var f struct {
						Message string `json:"fault_message"`
					}
					_ = json.Unmarshal(op.Result, &f)
					return fmt.Errorf("operation %d %s: %s", id, op.Status, f.Message)
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation %d: %w", id, ctx.Err())
		case <-ticker.C:
		}
	}
}

// stopNativeGuest asks the supervisor to kill any restoring or restored child
// and waits briefly for it to settle; it is best-effort.
func stopNativeGuest(socket string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := nativeFCAccepted(ctx, socket, http.MethodPut, "/actions", map[string]string{"action_type": "ForceStop"}); err != nil {
		return
	}
	_ = waitNativeFCState(ctx, socket, 10*time.Second, "Exited", "NotStarted", "Failed")
}

// terminateNativeSupervisor stops a supervisor this daemon no longer parents.
// Callers must have verified ownership by PID and VM-specific socket.
func terminateNativeSupervisor(pid int, socket string) {
	stopNativeGuest(socket)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(snapshotTermGrace)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = os.Remove(socket)
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		slog.Info("snapshot recovery: terminated orphan restore supervisor", "pid", pid)
	}
}
