//go:build linux

package vm

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/AitorConS/jerboa/internal/network"
)

// adoptPollInterval is how often a re-adopted VM's process is polled for exit.
const adoptPollInterval = 2 * time.Second

// processAlive reports whether a process with the given PID currently exists.
// Signal 0 performs an existence/permission check without delivering a signal.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processOwnsVM verifies that the live process pid really is the QEMU instance
// for vmID, guarding against PID reuse after the original process died. The
// VM's QMP socket path is unique per VM and appears on the QEMU command line,
// so matching it confirms ownership.
func processOwnsVM(pid int, vmID string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte(qmpSocketPath(vmID)))
}

// recoverVM decides how to restore a VM the store recorded as running/starting.
//
// If its process survived the daemon — verified by PID liveness and ownership —
// the VM is re-adopted: its process handle and QMP address are reattached so
// stop/signal keep working, and a goroutine polls for the process's eventual
// exit. Otherwise the VM is marked stopped and flagged as daemon-recovered
// (the historical behavior). A non-positive pid always takes the stopped path.
//
// It returns true when the in-memory state diverged from the store (the dead
// path) and must be persisted by the caller. The caller is responsible for
// calling Store.Save outside any restore lock to avoid a self-deadlock.
func recoverVM(s Store, v *VM, pid int) (needsSave bool) {
	if pid > 0 && processAlive(pid) && processOwnsVM(pid, v.ID) {
		p, _ := os.FindProcess(pid) // never returns an error on Unix
		v.State = StateRunning
		v.pid = pid
		v.proc = &osProcess{p: p}
		v.qmpAddr = "unix:" + qmpSocketPath(v.ID)
		slog.Info("restore: re-adopted running vm", "vm_id", v.ID, "pid", pid)
		go adoptMonitor(s, v)
		return false
	}
	slog.Info("restore: marking vm as stopped (daemon restart)", "vm_id", v.ID)
	v.State = StateStopped
	now := time.Now()
	v.StoppedAt = &now
	v.DaemonRecovered = true
	close(v.done)
	// The process died with the previous daemon; its network resources may be
	// orphaned. Best-effort reconcile so stale TAPs/forwards don't leak.
	go reconcileNetwork(v.ID, v.Cfg)
	return true
}

// reconcileNetwork tears down the per-VM network resources a dead VM may have
// left behind: its TAP port forwards and the TAP's attachment to the bridge. It
// deliberately does not destroy the bridge itself — bridges are shared across
// VMs (e.g. the default jerboa-br0), so removing one here could break other
// running or re-adopted VMs. Best-effort: failures are logged at debug level.
func reconcileNetwork(vmID string, cfg Config) {
	if cfg.NetworkName == "" {
		return
	}
	if len(cfg.PortMaps) > 0 {
		if err := network.TeardownTAPPortForwarding(cfg.NetworkName, cfg.IPAddress, toNetworkPortForwards(cfg.PortMaps)); err != nil {
			slog.Debug("reconcile: tear down port forwarding", "vm_id", vmID, "err", err)
		}
	}
	if cfg.GatewayIP != "" {
		if err := network.DetachTAP(cfg.NetworkName); err != nil {
			slog.Debug("reconcile: detach TAP", "vm_id", vmID, "err", err)
		}
	}
}

// adoptMonitor watches a re-adopted VM's process and transitions it to stopped
// once the process exits, cleaning up its QMP socket. It replaces the normal
// exec.Cmd-based monitor, which is unavailable for a process the daemon did not
// fork itself; automatic restart policies do not apply to adopted VMs.
func adoptMonitor(s Store, v *VM) {
	for {
		time.Sleep(adoptPollInterval)
		v.mu.RLock()
		pid := v.pid
		qmpAddr := v.qmpAddr
		v.mu.RUnlock()
		if processAlive(pid) {
			continue
		}
		removeQMPSocket(qmpAddr)
		// The adopted process is gone; tear down its (bridge-safe) network
		// resources just like the normal monitor would on a clean exit.
		reconcileNetwork(v.ID, v.Cfg)
		if err := v.transition(StateStopped); err != nil {
			slog.Debug("adopt monitor: transition to stopped", "vm_id", v.ID, "err", err)
		}
		_ = s.Save(v)
		return
	}
}
