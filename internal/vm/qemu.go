//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AitorConS/jerboa/internal/network"
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

// WithKernel selects the ARM64 kernel used for direct boot.
func WithKernel(path string) Option {
	return func(m *QEMUManager) { m.kernelPath = path }
}

// WithGuestDNS supplies network-scoped responses for native guest DNS.
func WithGuestDNS(fn func([]byte, string) ([]byte, error)) Option {
	return func(m *QEMUManager) { m.guestDNS = fn }
}

// WithMetrics injects a sink for VM lifecycle counters.
func WithMetrics(s MetricsSink) Option {
	return func(m *QEMUManager) { m.metrics = s }
}

// QEMUManager implements Manager by spawning qemu-system-x86_64 processes.
type QEMUManager struct {
	nativeMu    sync.Mutex
	nativeState nativeNetworkState
	guestDNS    func([]byte, string) ([]byte, error)
	store       Store
	qemuBin     string
	kernelPath  string
	mkCmd       CommandFunc
	hchecker    *HealthChecker
	metrics     MetricsSink
	// applyLimits places the hypervisor process into a per-VM cgroup with the
	// requested CPU/memory limits, returning an error the caller turns into a
	// failed Start. Defaults to defaultApplyLimits; tests override it.
	applyLimits func(v *VM, pid int) error
}

// NewQEMUManager returns a QEMUManager using qemuBin as the QEMU executable.
func NewQEMUManager(qemuBin string, opts ...Option) *QEMUManager {
	m := &QEMUManager{
		store:       NewMemoryStore(),
		qemuBin:     qemuBin,
		mkCmd:       defaultCommandFunc,
		hchecker:    NewHealthChecker(),
		applyLimits: defaultApplyLimits,
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
	if err := validateVMConfig(cfg); err != nil {
		return nil, fmt.Errorf("qemu manager create: %w", err)
	}
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
	if err := validateHostConfig(v.Cfg, m.kernelPath); err != nil {
		return fmt.Errorf("qemu start %s: %w", id, err)
	}
	if err := validatePortNetwork(v.Cfg); err != nil {
		return fmt.Errorf("qemu start %s: %w", id, err)
	}
	if err := v.transition(StateStarting); err != nil {
		return fmt.Errorf("qemu start %s: %w", id, err)
	}

	// QMP travels over a per-VM Unix domain socket (instead of OS signals) for
	// graceful shutdown and exec signal delivery. A Unix socket is bound
	// directly by QEMU, eliminating the TCP ephemeral-port race where another
	// process could grab the port between probing it and QEMU binding.
	qmpAddr := "unix:" + qmpSocketPath(v.ID)
	removeQMPSocket(qmpAddr) // clear any stale socket from a previous run

	// Give this VM its own uniquely named TAP device. Several VMs can share a
	// network (and its bridge), but a TAP can be enslaved to only one VM, so the
	// device name must be per-VM rather than per-network.
	if v.Cfg.NetworkName != "" && v.Cfg.TapName == "" {
		v.Cfg.TapName = tapDeviceName(v.ID)
	}

	cleanup, err := m.prepareHostVM(v)
	if err != nil {
		_ = v.transition(StateStopped)
		return fmt.Errorf("qemu native setup: %w", err)
	}
	launched := false
	defer func() {
		if !launched && cleanup != nil {
			cleanup()
		}
	}()
	cmd := m.buildCmd(ctx, v.Cfg, qmpAddr)

	// Wire the tap into the bridge before launching QEMU. QEMU runs with
	// script=no,downscript=no, so it will not create or bridge the tap itself;
	// the device must already exist, be enslaved to the bridge, and be up by
	// the time the guest brings its interface online. Doing this after
	// cmd.Start() raced QEMU's own tap creation and left the tap down and
	// unbridged, so the guest's static IP was never reachable.
	if v.Cfg.NetworkName != "" && runtime.GOOS == "linux" {
		if err := setupTAPNetwork(v.Cfg); err != nil {
			slog.Warn("qemu start: tap network setup failed", "vm_id", id, "err", err)
		}
	}

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
	// Keep QEMU's own stderr in a dedicated buffer so the monitor can tell a
	// clean guest exit(0) from QEMU's own exit(1) — both surface as process
	// status 1 because isa-debug-exit calls exit((code<<1)|1) directly, with no
	// QMP SHUTDOWN event, so only QEMU's error diagnostic on stderr distinguishes
	// them. Still tee stderr into the shared log/attach stream so the diagnostics
	// stay visible to `jerboa logs` and `--attach` exactly as before.
	cmd.Stderr = io.MultiWriter(stdout, &v.qemuErrBuf)
	if err := cmd.Start(); err != nil {
		if tErr := v.transition(StateStopped); tErr != nil {
			return fmt.Errorf("qemu start %s: launch: %w; also failed to stop: %w", id, err, tErr)
		}
		return fmt.Errorf("qemu start %s: launch: %w", id, err)
	}
	launched = true
	now := time.Now()
	v.mu.Lock()
	v.proc = &osProcess{cmd.Process}
	v.pid = cmd.Process.Pid
	v.StartedAt = &now
	v.qmpAddr = qmpAddr
	v.mu.Unlock()
	if newStatsCollector != nil {
		collector := newStatsCollector(cmd.Process.Pid, v)
		v.SetStatsProvider(collector.Collect)
	}

	// abort tears down a process whose post-launch setup failed, before the VM is
	// committed as a monitored "running" instance. It reuses the normal monitor
	// with the explicit-stop flag set so teardown (tap, cgroup) and
	// restart-suppression match a clean stop; the failure is returned to the
	// caller instead of being swallowed as a WARN.
	abort := func() {
		v.SetExplicitStop()
		_ = cmd.Process.Kill()
		go m.monitor(v, cmd)
	}

	// Resource limits are applied only when explicitly requested. If they cannot
	// be honored the run FAILS rather than launching a VM without the isolation
	// the user asked for (a silent over-commit that can starve the host).
	if v.Cfg.CPUShares > 0 || v.Cfg.MemoryMax > 0 {
		if err := m.applyLimits(v, cmd.Process.Pid); err != nil {
			abort()
			return fmt.Errorf("qemu start %s: %w", id, err)
		}
	}
	if err := v.transition(StateRunning); err != nil {
		// Tear the process down through the same monitor path as the other
		// post-launch failures: a bare Kill would leave cmd.Wait unrun (zombie)
		// and leak the QMP socket, the tap device, and the cgroup applied above.
		abort()
		return fmt.Errorf("qemu start %s: %w", id, err)
	}
	_ = m.store.Save(v)

	// A published port that never binds is a broken run, so a port-forwarder
	// start failure (host port already in use, permission denied, …) FAILS the
	// run instead of leaving a "running" VM whose published port silently does
	// not work.
	if len(v.Cfg.PortMaps) > 0 && runtime.GOOS != "darwin" {
		fwd, fwdErr := network.StartForwarder(v.Cfg.IPAddress, toNetworkPortForwards(v.Cfg.PortMaps))
		if fwdErr != nil {
			abort()
			return fmt.Errorf("qemu start %s: publish ports: %w", id, fwdErr)
		}
		v.mu.Lock()
		v.portFwd = fwd
		v.mu.Unlock()
	}

	go m.monitor(v, cmd)
	m.hchecker.Start(ctx, v)
	// Watch the boot log for a volume that fails to mount, so the silent
	// data-loss case surfaces as a visible warning instead of vanishing writes.
	if len(v.Cfg.Volumes) > 0 {
		go watchVolumeMounts(ctx, v)
	}
	return nil
}

// Stop gracefully shuts down the VM: sends SIGTERM, waits up to gracePeriod,
// then kills if still running.
func (m *QEMUManager) Stop(ctx context.Context, id string) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("qemu stop %s: %w", id, err)
	}
	// Stopping an already-stopped VM is a no-op (idempotent), like `docker stop`.
	if v.GetState() == StateStopped {
		return nil
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
	// Killing an already-stopped VM is a no-op (idempotent).
	if v.GetState() == StateStopped {
		return nil
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
	if runtime.GOOS == "darwin" {
		return m.buildNativeCmd(ctx, cfg, qmpAddr)
	}
	// snapshot=on makes the boot disk copy-on-write: QEMU keeps guest writes in a
	// temporary overlay and discards them on exit, leaving the base image
	// pristine. This matches the documented model — the root filesystem is
	// ephemeral scratch space (lost on stop) and durable data belongs on a
	// volume. It also lets the same image back several VMs at once (the base is
	// opened read-only, so there is no write-lock contention), and it keeps
	// runtime state like a database's postmaster.pid/lock files from persisting
	// into the image and breaking the next boot. Attached volumes are separate
	// -drive entries without snapshot, so their data still persists.
	driveArg := "file=" + cfg.ImagePath + ",format=raw,if=virtio,snapshot=on"
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
		// isa-debug-exit surfaces the GUEST's exit code to the host. Nanos writes
		// the process exit status to port 0x501 (QEMU_HALT) on shutdown; with this
		// device QEMU then terminates with process status (guestCode<<1)|1, so the
		// monitor can tell a clean guest exit from a crash. Without it a guest
		// exit(N) just powers the VM off and QEMU exits 0, hiding the crash and
		// making --restart on-failure never fire. See guestExitCode.
		"-device", "isa-debug-exit,iobase=0x501,iosize=1",
	}
	args = append(args, kvmAccelArgs()...)
	if cfg.CPUs > 0 {
		args = append(args, "-smp", fmt.Sprintf("%d", cfg.CPUs))
	}

	args = append(args, buildNetArgs(cfg)...)
	args = append(args, buildEnvArgs(cfg.Env)...)
	args = append(args, buildNetworkCfgArgs(cfg)...)
	args = append(args, buildVolumeArgs(cfg.Volumes)...)
	args = append(args, buildMountArgs(cfg.Volumes)...)
	if qmpAddr != "" {
		// qmpAddr already carries its scheme ("unix:<path>" or "tcp:host:port").
		args = append(args, "-qmp", qmpAddr+",server,nowait")
	}

	cmd := m.mkCmd(ctx, m.qemuBin, args...)
	return cmd
}

var (
	kvmProbeOnce sync.Once
	kvmProbeArgs []string
)

// kvmAccelArgs returns the QEMU acceleration arguments, probed once per
// process. Opening /dev/kvm is the authoritative check — the node can exist
// while being inaccessible to the daemon. With KVM the guest runs with
// hardware virtualization and the host CPU model; without it QEMU silently
// falls back to TCG software emulation, which is an order of magnitude slower
// for guest CPU work.
func kvmAccelArgs() []string {
	kvmProbeOnce.Do(func() {
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			slog.Warn("KVM unavailable; falling back to TCG emulation", "err", err)
			return
		}
		_ = f.Close()
		kvmProbeArgs = []string{"-enable-kvm", "-cpu", "host"}
	})
	return kvmProbeArgs
}

// validatePortNetwork rejects port maps without a TAP network and guest IP.
// Port publishing requires a guest IP on a TAP interface (handled by the
// userspace forwarder); there is no SLIRP fallback, so PortMaps without a
// NetworkName cannot work, and without an IPAddress the forwarder has no
// dial target. Failing here surfaces the misconfiguration at Start instead
// of booting a VM whose published ports silently never listen.
func validatePortNetwork(cfg Config) error {
	if runtime.GOOS == "darwin" {
		return nil
	}
	if len(cfg.PortMaps) == 0 {
		return nil
	}
	if cfg.NetworkName == "" {
		return fmt.Errorf("--port requires --network <name>: SLIRP user-mode networking is no longer supported")
	}
	if cfg.IPAddress == "" {
		return fmt.Errorf("port publishing requires a guest IP: pass one explicitly or let the daemon allocate it from network %q", cfg.NetworkName)
	}
	return nil
}

// buildNetArgs returns the QEMU network arguments for cfg.
// Networking is TAP only (explicit NetworkName); otherwise the VM has no
// network. Port publishing is handled by a userspace forwarder, not SLIRP, so
// QEMU and Firecracker share one networking model. PortMaps require a TAP
// network and are rejected earlier (Start) when NetworkName is empty.
func buildNetArgs(cfg Config) []string {
	if cfg.NetworkName != "" {
		dev := "virtio-net-pci,netdev=net0"
		// Give each VM a stable, unique MAC derived from its IP. QEMU's default
		// is a fixed MAC, which collides when several VMs share a bridge.
		if cfg.IPAddress != "" {
			dev += ",mac=" + guestMACFromIP(cfg.IPAddress)
		}
		return []string{
			// script=no/downscript=no: the daemon wires the tap into the bridge
			// itself, so QEMU must not run /etc/qemu-ifup (which fails with
			// "no bridge found"). Matches ops' TAP setup.
			"-netdev", "tap,id=net0,ifname=" + cfg.tapDevice() + ",script=no,downscript=no",
			"-device", dev,
		}
	}
	return []string{"-net", "none"}
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

// buildMountArgs encodes volume mount points as a QEMU fw_cfg entry. The guest
// kernel reads "opt/uni/mounts" (mounts_inject_from_fw_cfg) and mounts each
// volume — matched by its TFS label — at the requested guest path. Format: one
// "LABEL:/path" entry per line. Volumes without a label or guest path are
// attached as bare block devices only and skipped here.
func buildMountArgs(vols []VolumeMount) []string {
	var entries []string
	for _, vol := range vols {
		if vol.Label == "" || vol.GuestPath == "" {
			continue
		}
		entries = append(entries, vol.Label+":"+vol.GuestPath)
	}
	if len(entries) == 0 {
		return nil
	}
	encoded := strings.Join(entries, "\n")
	return []string{"-fw_cfg", "name=opt/uni/mounts,string=" + escapeFwCfgValue(encoded)}
}

// buildEnvArgs encodes environment variables as QEMU fw_cfg entries.
// The guest kernel reads them from the "opt/uni/env" fw_cfg key.
// Each call produces zero or one -fw_cfg argument; format is "KEY=VALUE\n" joined.
func buildEnvArgs(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	encoded := strings.Join(env, "\n")
	return []string{"-fw_cfg", "name=opt/uni/env,string=" + escapeFwCfgValue(encoded)}
}

// escapeFwCfgValue escapes a value for use inside a QEMU -fw_cfg "string=" field.
// QEMU's option parser treats commas as separators, so a literal comma must be
// doubled; QEMU collapses ",," back to "," before exposing the value to the
// guest. Without this, a value like "10.0.0.2/24,10.0.0.1" is split and the part
// after the comma is rejected as an invalid option.
func escapeFwCfgValue(s string) string {
	return strings.ReplaceAll(s, ",", ",,")
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
	return []string{"-fw_cfg", "name=opt/uni/network,string=" + escapeFwCfgValue(netCfg)}
}

func (m *QEMUManager) monitor(v *VM, cmd *exec.Cmd) {
	defer recoverGoroutine("qemu monitor", v.ID)
	exitErr := cmd.Wait()
	now := time.Now()
	v.mu.Lock()
	v.StoppedAt = &now
	if v.logPipeWriter != nil {
		_ = v.logPipeWriter.Close()
	}
	explicitStop := v.explicitStop
	qmpAddr := v.qmpAddr
	cleanup := v.hostCleanup
	v.hostCleanup = nil
	fwd := v.portFwd
	v.portFwd = nil
	v.mu.Unlock()
	removeQMPSocket(qmpAddr)
	if cleanup != nil {
		cleanup()
	}
	if fwd != nil {
		fwd.Close()
	}
	v.mu.RLock()
	cg := v.cgroupMgr
	v.mu.RUnlock()
	if cg != nil {
		if err := cg.Remove(); err != nil {
			slog.Warn("qemu monitor: cgroup remove failed", "vm_id", v.ID, "err", err)
		}
	}
	if v.Cfg.NetworkName != "" && runtime.GOOS == "linux" {
		// Mirror setupTAPNetwork's creation: detach and delete the persistent
		// tap. The bridge is intentionally left in place — it may be shared by
		// other VMs and (for `jerboa network`-managed bridges) is owned by the
		// network's lifecycle, not the VM's.
		tap := v.Cfg.tapDevice()
		if err := network.DetachTAP(tap); err != nil {
			slog.Debug("qemu monitor: detach tap", "vm_id", v.ID, "err", err)
		}
		if err := network.DeleteTAPDevice(tap); err != nil {
			slog.Debug("qemu monitor: delete tap", "vm_id", v.ID, "err", err)
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
		// qemuErrBuf holds QEMU's own stderr, used to disambiguate a guest
		// exit(0) from QEMU's own exit(1) (both surface as process status 1).
		if runtime.GOOS == "darwin" && !v.Cfg.EmulateX86 {
			shouldRestart = exitErr != nil
		} else {
			shouldRestart = isFailureExit(exitErr, string(v.qemuErrBuf.Bytes()))
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

	if m.metrics != nil {
		m.metrics.RecordRestart()
	}

	ctx := context.Background()
	cfg := old.Cfg
	newVM, err := m.store.Create(cfg)
	if err != nil {
		slog.Error("monitor: failed to create replacement vm", "vm_id", old.ID, "err", err)
		if m.metrics != nil {
			m.metrics.RecordError()
		}
		return
	}
	newVM.mu.Lock()
	newVM.RestartCount = restartCount + 1
	newVM.mu.Unlock()
	_ = m.store.Save(newVM)

	if err := m.Start(ctx, newVM.ID); err != nil {
		slog.Error("monitor: failed to start replacement vm", "vm_id", newVM.ID, "err", err)
		if m.metrics != nil {
			m.metrics.RecordError()
		}
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
			BindAddr:  pm.BindAddr,
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
