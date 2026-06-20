package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/network"
)

var defaultCommandFunc CommandFunc = exec.CommandContext

const gracePeriod = 30 * time.Second

// CommandFunc builds an exec.Cmd. Defaults to exec.CommandContext; replaceable in tests.
type CommandFunc func(ctx context.Context, name string, args ...string) *exec.Cmd

// Option configures a QEMUManager.
type Option func(*QEMUManager)

// WithCommandFunc injects a custom command builder (for tests).
func WithCommandFunc(fn CommandFunc) Option {
	return func(m *QEMUManager) { m.mkCmd = fn }
}

// WithStore injects a custom Store implementation (e.g. FileStore for persistence).
func WithStore(s Store) Option {
	return func(m *QEMUManager) { m.store = s }
}

// QEMUManager implements Manager by spawning qemu-system-x86_64 processes.
type QEMUManager struct {
	store    Store
	qemuBin  string
	mkCmd    CommandFunc
	hchecker *HealthChecker
}

// NewQEMUManager returns a QEMUManager using qemuBin as the QEMU executable.
func NewQEMUManager(qemuBin string, opts ...Option) *QEMUManager {
	m := &QEMUManager{
		store:    NewMemoryStore(),
		qemuBin:  qemuBin,
		mkCmd:    defaultCommandFunc,
		hchecker: NewHealthChecker(),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Store returns the underlying Store for lifecycle operations like Restore.
func (m *QEMUManager) Store() Store { return m.store }

// Create registers a new VM with the given config.
func (m *QEMUManager) Create(_ context.Context, cfg Config) (*VM, error) {
	v, err := m.store.Create(cfg)
	if err != nil {
		return nil, fmt.Errorf("qemu manager create: %w", err)
	}
	return v, nil
}

// Start launches the QEMU process for the VM identified by id.
// The ctx parameter controls the command lifecycle; canceling ctx will kill
// the QEMU process via exec.CommandContext.
func (m *QEMUManager) Start(ctx context.Context, id string) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("qemu start %s: %w", id, err)
	}
	if err := v.transition(StateStarting); err != nil {
		return fmt.Errorf("qemu start %s: %w", id, err)
	}

	// Find a free port for QMP before building the QEMU command args.
	// QMP over TCP is used cross-platform (instead of OS signals) for
	// graceful shutdown and exec signal delivery.
	qmpAddr := ""
	if port, portErr := freePort(); portErr == nil {
		qmpAddr = fmt.Sprintf("127.0.0.1:%d", port)
	} else {
		slog.Warn("qemu start: cannot find free QMP port; stop/signal may fall back to OS signals", "vm_id", id, "err", portErr)
	}

	cmd := m.buildCmd(ctx, v.Cfg, qmpAddr)

	var stdout io.Writer = &v.logBuf
	if v.Cfg.Attach {
		pr, pw := io.Pipe()
		v.mu.Lock()
		v.logPipeReader = pr
		v.logPipeWriter = pw
		v.mu.Unlock()
		stdout = io.MultiWriter(&v.logBuf, pw)
	}

	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Start(); err != nil {
		if tErr := v.transition(StateStopped); tErr != nil {
			return fmt.Errorf("qemu start %s: launch: %w; also failed to stop: %w", id, err, tErr)
		}
		return fmt.Errorf("qemu start %s: launch: %w", id, err)
	}
	now := time.Now()
	v.mu.Lock()
	v.proc = &osProcess{cmd.Process}
	v.StartedAt = &now
	v.qmpAddr = qmpAddr
	v.mu.Unlock()
	if newStatsCollector != nil {
		v.SetStatsProvider(func() RuntimeStats {
			return newStatsCollector(cmd.Process.Pid, v).Collect()
		})
	}
	if v.Cfg.CPUShares > 0 || v.Cfg.MemoryMax > 0 {
		if IsCgroupV2Available() {
			cg := NewCgroupManager(v.ID)
			if err := cg.Apply(cmd.Process.Pid, CgroupLimit{
				CPUShares: v.Cfg.CPUShares,
				MemoryMax: v.Cfg.MemoryMax,
			}); err != nil {
				slog.Warn("qemu start: cgroup apply failed", "vm_id", id, "err", err)
			}
			v.mu.Lock()
			v.cgroupMgr = cg
			v.mu.Unlock()
		} else {
			slog.Warn("qemu start: cgroup v2 not available, skipping resource limits", "vm_id", id)
		}
	}
	if err := v.transition(StateRunning); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("qemu start %s: %w", id, err)
	}
	_ = m.store.Save(v)
	if v.Cfg.NetworkName != "" && len(v.Cfg.PortMaps) > 0 {
		if err := network.SetupTAPPortForwarding(v.Cfg.NetworkName, v.Cfg.IPAddress, toNetworkPortForwards(v.Cfg.PortMaps)); err != nil {
			slog.Warn("qemu start: failed to set up TAP port forwarding", "vm_id", id, "err", err)
		}
	}
	if v.Cfg.NetworkName != "" && v.Cfg.GatewayIP != "" {
		bridgeName := v.Cfg.BridgeName
		if bridgeName == "" {
			bridgeName = "uni-br0"
		}
		mask := v.Cfg.SubnetMask
		if mask == "" {
			mask = "24"
		}
		cidr := v.Cfg.GatewayIP + "/" + mask
		if err := network.CreateBridge(network.BridgeConfig{Name: bridgeName, CIDR: cidr}); err != nil {
			slog.Warn("qemu start: failed to create bridge", "bridge", bridgeName, "err", err)
		}
		if err := network.AttachTAP(v.Cfg.NetworkName, bridgeName); err != nil {
			slog.Warn("qemu start: failed to attach TAP to bridge", "tap", v.Cfg.NetworkName, "bridge", bridgeName, "err", err)
		}
	}
	go m.monitor(v, cmd)
	m.hchecker.Start(ctx, v)
	return nil
}

// Stop gracefully shuts down the VM: sends SIGTERM, waits up to gracePeriod,
// then kills if still running.
func (m *QEMUManager) Stop(ctx context.Context, id string) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("qemu stop %s: %w", id, err)
	}
	if err := v.transition(StateStopping); err != nil {
		return fmt.Errorf("qemu stop %s: %w", id, err)
	}
	_ = m.store.Save(v)
	m.hchecker.Stop(v.ID)
	v.SetExplicitStop()
	v.mu.RLock()
	proc := v.proc
	qmpAddr := v.qmpAddr
	v.mu.RUnlock()
	if proc == nil {
		return nil
	}

	// Try graceful guest shutdown via QMP (cross-platform: TCP-based).
	// Falls back to OS SIGTERM → kill for backwards compat with old VMs / test fakes.
	qmpOK := false
	if qmpAddr != "" {
		if err := qmpDo(qmpAddr, "system_powerdown"); err == nil {
			qmpOK = true
		} else {
			slog.Debug("qemu stop: qmp powerdown failed, using OS signal", "vm_id", id, "err", err)
		}
	}
	if !qmpOK {
		if err := proc.signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			// SIGTERM not supported on this platform (e.g. Windows); fall back to kill.
			slog.Debug("qemu stop: sigterm unsupported, falling back to kill", "vm_id", id, "err", err)
			if killErr := proc.kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				return fmt.Errorf("qemu stop %s: kill after failed sigterm: %w", id, killErr)
			}
			return nil
		}
	}
	select {
	case <-v.Done():
		return nil
	case <-time.After(gracePeriod):
	case <-ctx.Done():
	}
	if err := proc.kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("qemu stop %s: kill after grace: %w", id, err)
	}
	return nil
}

// Kill immediately sends SIGKILL to the VM process.
func (m *QEMUManager) Kill(_ context.Context, id string) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("qemu kill %s: %w", id, err)
	}
	if err := v.transition(StateStopping); err != nil {
		return fmt.Errorf("qemu kill %s: %w", id, err)
	}
	_ = m.store.Save(v)
	m.hchecker.Stop(v.ID)
	v.SetExplicitStop()
	v.mu.RLock()
	proc := v.proc
	v.mu.RUnlock()
	if proc != nil {
		if err := proc.kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("qemu kill %s: %w", id, err)
		}
	}
	return nil
}

// Signal sends sig to the VM. SIGKILL terminates the QEMU host process immediately
// (cross-platform). All other signals request graceful guest shutdown via QEMU QMP
// (system_powerdown sends an ACPI power-button event); if QMP is unavailable the
// call falls back to an OS-level signal (Linux/macOS only).
func (m *QEMUManager) Signal(_ context.Context, id string, sig os.Signal) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("qemu signal %s: %w", id, err)
	}
	v.mu.RLock()
	proc := v.proc
	qmpAddr := v.qmpAddr
	v.mu.RUnlock()
	if proc == nil {
		return fmt.Errorf("qemu signal %s: no process", id)
	}
	// SIGKILL: immediately terminate the QEMU host process.
	// os.Process.Kill() is cross-platform (TerminateProcess on Windows).
	if sig == syscall.SIGKILL {
		if err := proc.kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("qemu signal %s: %w", id, err)
		}
		return nil
	}
	// For all other signals, try QMP system_powerdown (cross-platform).
	if qmpAddr != "" {
		if err := qmpDo(qmpAddr, "system_powerdown"); err == nil {
			return nil
		}
		slog.Debug("qemu signal: qmp failed, falling back to OS signal", "vm_id", id)
	}
	// Fallback: OS-level signal (Linux/macOS). Fails on Windows for non-Kill signals.
	if err := proc.signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("qemu signal %s: %w", id, err)
	}
	return nil
}

// Remove deletes a stopped VM from the registry.
func (m *QEMUManager) Remove(_ context.Context, id string) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("qemu remove %s: %w", id, err)
	}
	if st := v.GetState(); st != StateStopped {
		return fmt.Errorf("qemu remove %s: vm is %s, must be stopped first", id, st)
	}
	m.hchecker.Stop(v.ID)
	if err := m.store.Remove(v.ID); err != nil {
		return fmt.Errorf("qemu remove %s: %w", id, err)
	}
	return nil
}

// Get returns the VM with the given id, name, or ID prefix.
func (m *QEMUManager) Get(id string) (*VM, error) {
	v, err := m.store.Resolve(id)
	if err != nil {
		return nil, fmt.Errorf("qemu get %s: %w", id, err)
	}
	return v, nil
}

// List returns all registered VMs.
func (m *QEMUManager) List() []*VM {
	return m.store.List()
}

func (m *QEMUManager) buildCmd(ctx context.Context, cfg Config, qmpAddr string) *exec.Cmd {
	driveArg := "file=" + cfg.ImagePath + ",format=raw,if=virtio"
	if cfg.DiskIOPS > 0 {
		driveArg += fmt.Sprintf(",throttling.iops-total=%d", cfg.DiskIOPS)
	}
	if cfg.DiskBPS > 0 {
		driveArg += fmt.Sprintf(",throttling.bps-total=%d", cfg.DiskBPS)
	}
	args := []string{
		"-m", cfg.Memory,
		"-drive", driveArg,
		"-nographic",
		"-no-reboot",
	}
	if cfg.CPUs > 0 {
		args = append(args, "-smp", fmt.Sprintf("%d", cfg.CPUs))
	}

	args = append(args, buildNetArgs(cfg)...)
	args = append(args, buildEnvArgs(cfg.Env)...)
	args = append(args, buildNetworkCfgArgs(cfg)...)
	args = append(args, buildVolumeArgs(cfg.Volumes)...)
	if qmpAddr != "" {
		// QMP over TCP: works on Linux, macOS, and Windows without admin privileges.
		args = append(args, "-qmp", "tcp:"+qmpAddr+",server,nowait")
	}

	cmd := m.mkCmd(ctx, m.qemuBin, args...)
	return cmd
}

// buildNetArgs returns the QEMU network arguments for cfg.
// Priority: TAP (explicit NetworkName) → SLIRP with hostfwd (PortMaps set) → no network.
func buildNetArgs(cfg Config) []string {
	if cfg.NetworkName != "" {
		return []string{
			"-netdev", "tap,id=net0,ifname=" + cfg.NetworkName,
			"-device", "virtio-net-pci,netdev=net0",
		}
	}
	if len(cfg.PortMaps) > 0 {
		return slirpNetArgs(cfg.PortMaps)
	}
	return []string{"-net", "none"}
}

// slirpNetArgs builds the SLIRP user-mode networking arguments with hostfwd rules.
func slirpNetArgs(ports []PortMap) []string {
	netdev := "user,id=net0"
	for _, pm := range ports {
		netdev += fmt.Sprintf(",hostfwd=%s::%d-:%d", pm.Protocol, pm.HostPort, pm.GuestPort)
	}
	return []string{
		"-netdev", netdev,
		"-device", "virtio-net-pci,netdev=net0",
	}
}

// buildVolumeArgs appends extra virtio-blk drives for each volume mount.
// Each volume gets its own drive index (starting at 1; index 0 is the boot disk).
func buildVolumeArgs(vols []VolumeMount) []string {
	var args []string
	for i, vol := range vols {
		drive := fmt.Sprintf("file=%s,format=raw,if=virtio,index=%d", vol.DiskPath, i+1)
		if vol.ReadOnly {
			drive += ",readonly=on"
		}
		args = append(args, "-drive", drive)
	}
	return args
}

// buildEnvArgs encodes environment variables as QEMU fw_cfg entries.
// The guest kernel reads them from the "opt/uni/env" fw_cfg key.
// Each call produces zero or one -fw_cfg argument; format is "KEY=VALUE\n" joined.
func buildEnvArgs(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	encoded := strings.Join(env, "\n")
	return []string{"-fw_cfg", "name=opt/uni/env,string=" + encoded}
}

// buildNetworkCfgArgs encodes static network configuration as a QEMU fw_cfg entry.
// The guest kernel reads it from the "opt/uni/network" key.
// Format: "IP/CIDR,GATEWAY" (e.g. "10.0.0.2/24,10.0.0.1").
// Only populated when IPAddress is set (TAP networking with static IP).
func buildNetworkCfgArgs(cfg Config) []string {
	if cfg.IPAddress == "" || cfg.GatewayIP == "" {
		return nil
	}
	netMask := cfg.SubnetMask
	if netMask == "" {
		netMask = "24"
	}
	netCfg := cfg.IPAddress + "/" + netMask + "," + cfg.GatewayIP
	return []string{"-fw_cfg", "name=opt/uni/network,string=" + netCfg}
}

func (m *QEMUManager) monitor(v *VM, cmd *exec.Cmd) {
	exitErr := cmd.Wait()
	now := time.Now()
	v.mu.Lock()
	v.StoppedAt = &now
	if v.logPipeWriter != nil {
		_ = v.logPipeWriter.Close()
	}
	explicitStop := v.explicitStop
	v.mu.Unlock()
	if v.Cfg.NetworkName != "" && len(v.Cfg.PortMaps) > 0 {
		if err := network.TeardownTAPPortForwarding(v.Cfg.NetworkName, v.Cfg.IPAddress, toNetworkPortForwards(v.Cfg.PortMaps)); err != nil {
			slog.Warn("qemu monitor: failed to tear down TAP port forwarding", "vm_id", v.ID, "err", err)
		}
	}
	v.mu.RLock()
	cg := v.cgroupMgr
	v.mu.RUnlock()
	if cg != nil {
		if err := cg.Remove(); err != nil {
			slog.Warn("qemu monitor: cgroup remove failed", "vm_id", v.ID, "err", err)
		}
	}
	if v.Cfg.NetworkName != "" && v.Cfg.GatewayIP != "" {
		bridgeName := v.Cfg.BridgeName
		if bridgeName == "" {
			bridgeName = "uni-br0"
		}
		if err := network.DetachTAP(v.Cfg.NetworkName); err != nil {
			slog.Warn("qemu monitor: failed to detach TAP from bridge", "tap", v.Cfg.NetworkName, "err", err)
		}
		if err := network.DestroyBridge(bridgeName); err != nil {
			slog.Warn("qemu monitor: failed to destroy bridge", "bridge", bridgeName, "err", err)
		}
	}
	if err := v.transition(StateStopped); err != nil {
		slog.Debug("monitor: transition to stopped", "vm_id", v.ID, "err", err)
	}
	_ = m.store.Save(v)
	m.hchecker.Stop(v.ID)

	if explicitStop {
		slog.Info("monitor: vm stopped explicitly, not restarting", "vm_id", v.ID)
		return
	}
	if v.Cfg.Restart.Policy == RestartNever || v.Cfg.Restart.Policy == "" {
		return
	}
	shouldRestart := false
	switch v.Cfg.Restart.Policy {
	case RestartAlways:
		shouldRestart = true
	case RestartOnFailure:
		if exitErr != nil {
			shouldRestart = true
		}
	}
	if !shouldRestart {
		slog.Info("monitor: vm exited normally, not restarting", "vm_id", v.ID, "policy", v.Cfg.Restart.Policy)
		return
	}
	maxRetries := v.Cfg.Restart.MaxRetries
	if maxRetries > 0 {
		v.mu.Lock()
		restartCount := v.RestartCount
		v.mu.Unlock()
		if restartCount >= maxRetries {
			slog.Info("monitor: max retries reached, not restarting", "vm_id", v.ID, "retries", restartCount, "max", maxRetries)
			return
		}
	}
	go m.restartVM(v)
}

// restartVM creates a replacement VM with the same config, removes the old
// one, and starts the replacement. Uses exponential backoff capped at 30s.
func (m *QEMUManager) restartVM(old *VM) {
	old.mu.Lock()
	restartCount := old.RestartCount
	old.mu.Unlock()

	backoff := time.Duration(1<<uint(restartCount)) * time.Second
	if backoff > 30*time.Second {
		backoff = 30 * time.Second
	}
	slog.Info("monitor: restarting vm", "vm_id", old.ID, "attempt", restartCount+1, "backoff", backoff)
	time.Sleep(backoff)

	ctx := context.Background()
	cfg := old.Cfg
	newVM, err := m.store.Create(cfg)
	if err != nil {
		slog.Error("monitor: failed to create replacement vm", "vm_id", old.ID, "err", err)
		return
	}
	newVM.mu.Lock()
	newVM.RestartCount = restartCount + 1
	newVM.mu.Unlock()
	_ = m.store.Save(newVM)

	if err := m.Start(ctx, newVM.ID); err != nil {
		slog.Error("monitor: failed to start replacement vm", "vm_id", newVM.ID, "err", err)
		return
	}
	slog.Info("monitor: replacement vm started", "old_id", old.ID, "new_id", newVM.ID)
	if err := m.store.Remove(old.ID); err != nil {
		slog.Warn("monitor: failed to remove old vm from store", "vm_id", old.ID, "err", err)
	}
}
func toNetworkPortForwards(pms []PortMap) []network.PortForward {
	out := make([]network.PortForward, len(pms))
	for i, pm := range pms {
		out[i] = network.PortForward{
			HostPort:  pm.HostPort,
			GuestPort: pm.GuestPort,
			Protocol:  string(pm.Protocol),
		}
	}
	return out
}

// osProcess wraps *os.Process to implement the package-private process interface.
type osProcess struct{ p *os.Process }

func (o *osProcess) kill() error {
	if err := o.p.Kill(); err != nil {
		return fmt.Errorf("kill process %d: %w", o.p.Pid, err)
	}
	return nil
}

func (o *osProcess) signal(sig os.Signal) error {
	if err := o.p.Signal(sig); err != nil {
		return fmt.Errorf("signal process %d (%v): %w", o.p.Pid, sig, err)
	}
	return nil
}
