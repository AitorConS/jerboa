//go:build linux

package vm

import (
	"bytes"
	"context"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AitorConS/jerboa/internal/network"
)

// FCOption configures a FirecrackerManager.
type FCOption func(*FirecrackerManager)

// WithFCStore injects a custom Store implementation.
func WithFCStore(s Store) FCOption {
	return func(m *FirecrackerManager) { m.store = s }
}

// WithFCCommandFunc injects a custom command builder (for tests).
func WithFCCommandFunc(fn CommandFunc) FCOption {
	return func(m *FirecrackerManager) { m.mkCmd = fn }
}

// FirecrackerManager implements Manager by spawning firecracker processes
// configured via a JSON config file and managed via the Firecracker REST API
// over a per-VM Unix socket.
//
// Limitations vs. QEMUManager:
//   - TAP networking only (like QEMU now): port maps require a NetworkName and
//     are rejected at Start otherwise. Publishing is done by the userspace
//     forwarder, shared with QEMU.
//   - DiskIOPS / DiskBPS throttling is not available (no Firecracker equivalent).
//   - On Windows, Firecracker runs inside WSL2; KVM must be available in WSL2.
//   - The kernel image must be a flat ELF vmlinux compatible with Firecracker
//     (different from the BIOS-bootable kernel.img used by QEMU).
type FirecrackerManager struct {
	store       Store
	fcBin       string
	kernelImage string
	mkCmd       CommandFunc
	hchecker    *HealthChecker
	// platform hooks — overridden on Windows to route through WSL2
	vmSockPath         func(id string) string            // socket path as seen by the firecracker process
	cfgPathForProcess  func(path string) string          // translates config file path for the FC process
	shutdownAPI        func(sockPath string) error       // calls Firecracker's SendCtrlAltDel API
	rewriteConfigPaths func(cfg *fcVMConfig)             // rewrites paths inside the FC JSON config
	vmmLogPath         func(id string) string            // path for Firecracker's --log-path arg
	readVMMLog         func(path string) ([]byte, error) // reads VMM log (may use wsl on Windows)
}

// NewFirecrackerManager returns a FirecrackerManager.
// fcBin is the path to the firecracker binary.
// kernelImage is the path to a Firecracker-compatible vmlinux ELF kernel.
func NewFirecrackerManager(fcBin, kernelImage string, opts ...FCOption) *FirecrackerManager {
	m := &FirecrackerManager{
		store:              NewMemoryStore(),
		fcBin:              fcBin,
		kernelImage:        kernelImage,
		mkCmd:              defaultCommandFunc,
		hchecker:           NewHealthChecker(),
		vmSockPath:         fcSocketPath,
		cfgPathForProcess:  func(p string) string { return p },
		shutdownAPI:        fcSendCtrlAltDel,
		rewriteConfigPaths: func(*fcVMConfig) {},
		vmmLogPath:         func(id string) string { return filepath.Join(os.TempDir(), "fc-"+id+"-vmm.log") },
		readVMMLog:         os.ReadFile,
	}
	platformInitFC(m)
	for _, o := range opts {
		o(m)
	}
	return m
}

// Store returns the underlying Store.
func (m *FirecrackerManager) Store() Store { return m.store }

// Create registers a new VM with the given config.
func (m *FirecrackerManager) Create(_ context.Context, cfg Config) (*VM, error) {
	v, err := m.store.Create(cfg)
	if err != nil {
		return nil, fmt.Errorf("firecracker create: %w", err)
	}
	return v, nil
}

// Start writes a Firecracker config file and launches the firecracker process.
// The VM boots immediately upon process start (no separate InstanceStart call needed).
func (m *FirecrackerManager) Start(ctx context.Context, id string) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("firecracker start %s: %w", id, err)
	}
	if err := validatePortNetwork(v.Cfg); err != nil {
		return fmt.Errorf("firecracker start %s: %w", id, err)
	}
	if err := v.transition(StateStarting); err != nil {
		return fmt.Errorf("firecracker start %s: %w", id, err)
	}

	// Give this VM its own uniquely named TAP device. Several VMs can share a
	// network (and its bridge), but a TAP can be enslaved to only one VM, so the
	// device name must be per-VM rather than per-network.
	if v.Cfg.NetworkName != "" && v.Cfg.TapName == "" {
		v.Cfg.TapName = tapDeviceName(v.ID)
	}

	// Firecracker, unlike QEMU, does not create the tap or wire the bridge: it
	// opens an existing tap by name. Set up the persistent tap + bridge here,
	// before launching, so the guest is reachable on the host network.
	if v.Cfg.NetworkName != "" {
		if err := setupTAPNetwork(v.Cfg); err != nil {
			slog.Warn("firecracker start: network setup failed", "vm_id", id, "err", err)
		}
	}

	sockPath := m.vmSockPath(id)
	cfgPath, err := m.writeFCConfig(id, v.Cfg)
	if err != nil {
		_ = v.transition(StateStopped)
		return fmt.Errorf("firecracker start %s: write config: %w", id, err)
	}

	// --log-path separates Firecracker's VMM log lines from stdout so only the
	// VM serial console reaches logBuf. If the VM crashes with empty logs,
	// monitor appends the VMM log so `jerboa logs` surfaces the error.
	vmmLog := m.vmmLogPath(id)
	// Firecracker opens --log-path without O_CREAT and aborts if the file is
	// missing ("Could not initialize logger: ... No such file or directory"),
	// so create it (and its directory) before launching.
	if err := ensureFile(vmmLog); err != nil {
		_ = v.transition(StateStopped)
		_ = os.Remove(cfgPath)
		return fmt.Errorf("firecracker start %s: create vmm log: %w", id, err)
	}
	cmd := m.mkCmd(ctx, m.fcBin, "--api-sock", sockPath, "--config-file", m.cfgPathForProcess(cfgPath), "--log-path", vmmLog)

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
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		_ = v.transition(StateStopped)
		_ = os.Remove(cfgPath)
		return fmt.Errorf("firecracker start %s: launch: %w", id, err)
	}

	now := time.Now()
	v.mu.Lock()
	v.proc = &osProcess{cmd.Process}
	v.StartedAt = &now
	v.mu.Unlock()

	if err := v.transition(StateRunning); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("firecracker start %s: %w", id, err)
	}
	_ = m.store.Save(v)

	if len(v.Cfg.PortMaps) > 0 {
		fwd, fwdErr := network.StartForwarder(v.Cfg.IPAddress, toNetworkPortForwards(v.Cfg.PortMaps))
		if fwdErr != nil {
			slog.Warn("firecracker start: failed to start port forwarder", "vm_id", id, "err", fwdErr)
		} else {
			v.mu.Lock()
			v.portFwd = fwd
			v.mu.Unlock()
		}
	}

	go m.monitor(v, cmd, sockPath, cfgPath, vmmLog)
	m.hchecker.Start(ctx, v)
	return nil
}

// Stop gracefully shuts down the VM via Firecracker's SendCtrlAltDel API action,
// falling back to SIGTERM → SIGKILL after gracePeriod.
func (m *FirecrackerManager) Stop(ctx context.Context, id string) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("firecracker stop %s: %w", id, err)
	}
	if err := v.transition(StateStopping); err != nil {
		return fmt.Errorf("firecracker stop %s: %w", id, err)
	}
	_ = m.store.Save(v)
	m.hchecker.Stop(v.ID)
	v.SetExplicitStop()

	v.mu.RLock()
	proc := v.proc
	v.mu.RUnlock()
	if proc == nil {
		return nil
	}

	if err := m.shutdownAPI(m.vmSockPath(id)); err != nil {
		slog.Debug("firecracker stop: SendCtrlAltDel failed, falling back to SIGTERM", "vm_id", id, "err", err)
		if sigErr := proc.signal(syscall.SIGTERM); sigErr != nil && !errors.Is(sigErr, os.ErrProcessDone) {
			_ = proc.kill()
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
		return fmt.Errorf("firecracker stop %s: kill after grace: %w", id, err)
	}
	return nil
}

// Kill immediately terminates the firecracker process.
func (m *FirecrackerManager) Kill(_ context.Context, id string) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("firecracker kill %s: %w", id, err)
	}
	if err := v.transition(StateStopping); err != nil {
		return fmt.Errorf("firecracker kill %s: %w", id, err)
	}
	_ = m.store.Save(v)
	m.hchecker.Stop(v.ID)
	v.SetExplicitStop()

	v.mu.RLock()
	proc := v.proc
	v.mu.RUnlock()
	if proc != nil {
		if err := proc.kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("firecracker kill %s: %w", id, err)
		}
	}
	return nil
}

// Signal sends sig to the VM. SIGKILL kills the host process immediately;
// all other signals trigger a graceful SendCtrlAltDel via the Firecracker API,
// falling back to an OS-level signal on failure.
func (m *FirecrackerManager) Signal(_ context.Context, id string, sig os.Signal) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("firecracker signal %s: %w", id, err)
	}
	v.mu.RLock()
	proc := v.proc
	v.mu.RUnlock()
	if proc == nil {
		return fmt.Errorf("firecracker signal %s: no process", id)
	}
	if sig == syscall.SIGKILL {
		if err := proc.kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("firecracker signal %s: %w", id, err)
		}
		return nil
	}
	if err := m.shutdownAPI(m.vmSockPath(id)); err != nil {
		slog.Debug("firecracker signal: SendCtrlAltDel failed, falling back to OS signal", "vm_id", id)
		if err := proc.signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("firecracker signal %s: %w", id, err)
		}
	}
	return nil
}

// Remove deletes a stopped VM from the registry.
func (m *FirecrackerManager) Remove(_ context.Context, id string) error {
	v, err := m.store.Resolve(id)
	if err != nil {
		return fmt.Errorf("firecracker remove %s: %w", id, err)
	}
	if st := v.GetState(); st != StateStopped {
		return fmt.Errorf("firecracker remove %s: vm is %s, must be stopped first", id, st)
	}
	m.hchecker.Stop(v.ID)
	if err := m.store.Remove(v.ID); err != nil {
		return fmt.Errorf("firecracker remove %s: %w", id, err)
	}
	return nil
}

// Get returns the VM with the given id, name, or ID prefix.
func (m *FirecrackerManager) Get(id string) (*VM, error) {
	v, err := m.store.Resolve(id)
	if err != nil {
		return nil, fmt.Errorf("firecracker get %s: %w", id, err)
	}
	return v, nil
}

// List returns all registered VMs.
func (m *FirecrackerManager) List() []*VM {
	return m.store.List()
}

// setupTAPNetwork creates the persistent tap device and (idempotently) the
// bridge, attaching the tap so the guest's static IP is reachable from the
// host. Both hypervisors must do this before the process launches: the tap has
// to exist and be bridged before the guest brings its interface up. (QEMU is
// told script=no,downscript=no so it consumes this pre-made tap instead of
// running /etc/qemu-ifup.)
func setupTAPNetwork(cfg Config) error {
	if err := network.CreateTAPDevice(cfg.tapDevice()); err != nil {
		return err
	}
	if cfg.GatewayIP == "" {
		return nil
	}
	bridgeName := cfg.BridgeName
	if bridgeName == "" {
		bridgeName = "jerboa-br0"
	}
	mask := cfg.SubnetMask
	if mask == "" {
		mask = "24"
	}
	if err := network.EnsureBridge(network.BridgeConfig{Name: bridgeName, CIDR: cfg.GatewayIP + "/" + mask}); err != nil {
		return err
	}
	return network.AttachTAP(cfg.tapDevice(), bridgeName)
}

func (m *FirecrackerManager) monitor(v *VM, cmd *exec.Cmd, sockPath, cfgPath, vmmLog string) {
	defer recoverGoroutine("firecracker monitor", v.ID)
	exitErr := cmd.Wait()
	now := time.Now()
	v.mu.Lock()
	v.StoppedAt = &now
	if v.logPipeWriter != nil {
		_ = v.logPipeWriter.Close()
	}
	explicitStop := v.explicitStop
	fwd := v.portFwd
	v.portFwd = nil
	v.mu.Unlock()
	if fwd != nil {
		fwd.Close()
	}
	if v.Cfg.NetworkName != "" {
		tap := v.Cfg.tapDevice()
		if err := network.DetachTAP(tap); err != nil {
			slog.Debug("firecracker monitor: detach tap", "vm_id", v.ID, "err", err)
		}
		if err := network.DeleteTAPDevice(tap); err != nil {
			slog.Debug("firecracker monitor: delete tap", "vm_id", v.ID, "err", err)
		}
	}

	// If the VM died with no serial output (e.g. Firecracker couldn't start the
	// microVM), surface the VMM log so `jerboa logs` shows the actual error.
	if exitErr != nil && len(v.logBuf.Bytes()) == 0 {
		if data, err := m.readVMMLog(vmmLog); err == nil && len(data) > 0 {
			_, _ = v.logBuf.Write([]byte("[firecracker error]\n"))
			_, _ = v.logBuf.Write(data)
		}
	}

	_ = os.Remove(sockPath)
	_ = os.Remove(cfgPath)
	_ = os.Remove(vmmLog)

	if err := v.transition(StateStopped); err != nil {
		slog.Debug("firecracker monitor: transition to stopped", "vm_id", v.ID, "err", err)
	}
	_ = m.store.Save(v)
	m.hchecker.Stop(v.ID)

	if explicitStop {
		return
	}
	if v.Cfg.Restart.Policy == RestartNever || v.Cfg.Restart.Policy == "" {
		return
	}
	shouldRestart := v.Cfg.Restart.Policy == RestartAlways ||
		(v.Cfg.Restart.Policy == RestartOnFailure && exitErr != nil)
	if !shouldRestart {
		return
	}
	if max := v.Cfg.Restart.MaxRetries; max > 0 {
		v.mu.RLock()
		count := v.RestartCount
		v.mu.RUnlock()
		if count >= max {
			slog.Info("firecracker monitor: max retries reached", "vm_id", v.ID, "retries", count)
			return
		}
	}
	go m.restartVM(v)
}

func (m *FirecrackerManager) restartVM(old *VM) {
	old.mu.RLock()
	restartCount := old.RestartCount
	old.mu.RUnlock()

	backoff := time.Duration(1<<uint(restartCount)) * time.Second
	if backoff > 30*time.Second {
		backoff = 30 * time.Second
	}
	slog.Info("firecracker monitor: restarting vm", "vm_id", old.ID, "attempt", restartCount+1, "backoff", backoff)
	time.Sleep(backoff)

	ctx := context.Background()
	newVM, err := m.store.Create(old.Cfg)
	if err != nil {
		slog.Error("firecracker monitor: failed to create replacement vm", "vm_id", old.ID, "err", err)
		return
	}
	newVM.mu.Lock()
	newVM.RestartCount = restartCount + 1
	newVM.mu.Unlock()
	_ = m.store.Save(newVM)

	if err := m.Start(ctx, newVM.ID); err != nil {
		slog.Error("firecracker monitor: failed to start replacement vm", "vm_id", newVM.ID, "err", err)
		return
	}
	slog.Info("firecracker monitor: replacement vm started", "old_id", old.ID, "new_id", newVM.ID)
	if err := m.store.Remove(old.ID); err != nil {
		slog.Warn("firecracker monitor: failed to remove old vm", "vm_id", old.ID, "err", err)
	}
}

// writeFCConfig generates and writes the Firecracker JSON config file for v.
// Returns the path to the written file.
func (m *FirecrackerManager) writeFCConfig(id string, cfg Config) (string, error) {
	memMiB, err := parseMiB(cfg.Memory)
	if err != nil {
		return "", fmt.Errorf("parse memory %q: %w", cfg.Memory, err)
	}
	cpus := cfg.CPUs
	if cpus <= 0 {
		cpus = 1
	}

	bootArgs := buildFCBootArgs(cfg)

	fcCfg := fcVMConfig{
		BootSource: fcBootSource{
			KernelImagePath: m.kernelImage,
			BootArgs:        bootArgs,
		},
		Drives: []fcDrive{
			{
				DriveID:      "rootfs",
				PathOnHost:   cfg.ImagePath,
				IsRootDevice: true,
				IsReadOnly:   false,
			},
		},
		MachineConfig: fcMachineConfig{
			VcpuCount:  cpus,
			MemSizeMib: memMiB,
		},
	}

	for i, vol := range cfg.Volumes {
		fcCfg.Drives = append(fcCfg.Drives, fcDrive{
			DriveID:      fmt.Sprintf("vol%d", i),
			PathOnHost:   vol.DiskPath,
			IsRootDevice: false,
			IsReadOnly:   vol.ReadOnly,
		})
	}

	if cfg.NetworkName != "" {
		fcCfg.NetworkInterfaces = []fcNetworkInterface{
			{
				IfaceID:     "eth0",
				HostDevName: cfg.tapDevice(),
				// Firecracker leaves the guest NIC MAC unset otherwise, and Nanos
				// ends up with an all-zero MAC that bridges drop (ARP never
				// resolves). Assign a stable locally-administered MAC, like ops.
				GuestMAC: guestMACFromIP(cfg.IPAddress),
			},
		}
	}

	m.rewriteConfigPaths(&fcCfg)
	data, err := json.MarshalIndent(fcCfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal firecracker config: %w", err)
	}
	cfgPath := fcConfigPath(id)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		return "", fmt.Errorf("write firecracker config: %w", err)
	}
	return cfgPath, nil
}

// buildFCBootArgs constructs the kernel boot_args string for Nanos on Firecracker.
// Nanos reads environment variables and network config from kernel boot arguments
// (unlike QEMU where fw_cfg is used).
func buildFCBootArgs(cfg Config) string {
	// Standard args required by Firecracker + Nanos.
	args := "console=ttyS0 reboot=k panic=1 pci=off"

	// Environment variables: Nanos reads "environment.KEY=VALUE" from boot args.
	for _, kv := range cfg.Env {
		args += " environment." + kv
	}

	// Static network config via boot args (TAP only), using the interface-scoped
	// form ops uses: "en1.ipaddr=… en1.netmask=… en1.gateway=…". Nanos'
	// cmdline_apply turns the "en1." prefix into a nested en1 tuple that
	// init_network_iface looks up directly (the bare root-level form only hits a
	// deprecated fallback that assigns the address but does not fully bring the
	// interface up). The mask must be dotted-quad, not a CIDR suffix.
	if cfg.NetworkName != "" && cfg.IPAddress != "" {
		mask := cfg.SubnetMask
		if mask == "" {
			mask = "24"
		}
		args += fmt.Sprintf(" en1.ipaddr=%s en1.netmask=%s", cfg.IPAddress, cidrToNetmask(mask))
		if cfg.GatewayIP != "" {
			args += fmt.Sprintf(" en1.gateway=%s", cfg.GatewayIP)
		}
	}

	// Volume mount points: Nanos reads "mounts.LABEL=/path" from boot args and,
	// via cmdline_apply, builds a nested "mounts" tuple that storage_set_mountpoints
	// consumes (init.c, applied before the unix process starts). Each attached
	// volume is matched to a mount by its TFS label. No fw_cfg is needed on
	// Firecracker — the cmdline is delivered directly, unlike QEMU.
	for _, vol := range cfg.Volumes {
		if vol.Label == "" || vol.GuestPath == "" {
			continue
		}
		args += fmt.Sprintf(" mounts.%s=%s", vol.Label, vol.GuestPath)
	}

	return args
}

// guestMACFromIP derives a stable, locally-administered unicast MAC from a
// guest IPv4 address (e.g. 10.100.0.6 -> "AA:FC:0A:64:00:06"). The AA:FC prefix
// is locally administered (matching ops' convention); the four IPv4 octets keep
// it unique per VM. Falls back to a fixed MAC if the IP cannot be parsed.
func guestMACFromIP(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return "AA:FC:00:00:00:01"
	}
	octets := make([]int, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return "AA:FC:00:00:00:01"
		}
		octets[i] = n
	}
	return fmt.Sprintf("AA:FC:%02X:%02X:%02X:%02X", octets[0], octets[1], octets[2], octets[3])
}

// cidrToNetmask converts a CIDR prefix string (e.g. "24") to a dotted-quad
// netmask (e.g. "255.255.255.0"). Invalid input falls back to a /24 mask.
func cidrToNetmask(cidr string) string {
	n, err := strconv.Atoi(cidr)
	if err != nil || n < 0 || n > 32 {
		n = 24
	}
	var mask uint32 = 0xffffffff << (32 - n)
	if n == 0 {
		mask = 0
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask))
}

// parseMiB converts a QEMU-style memory string ("256M", "1G", "512") to MiB.
func parseMiB(mem string) (int, error) {
	mem = strings.TrimSpace(mem)
	if mem == "" {
		return 128, nil
	}
	suffix := mem[len(mem)-1]
	switch suffix {
	case 'M', 'm':
		n, err := strconv.Atoi(mem[:len(mem)-1])
		if err != nil {
			return 0, fmt.Errorf("parseMiB: %w", err)
		}
		return n, nil
	case 'G', 'g':
		n, err := strconv.Atoi(mem[:len(mem)-1])
		if err != nil {
			return 0, fmt.Errorf("parseMiB: %w", err)
		}
		return n * 1024, nil
	case 'K', 'k':
		n, err := strconv.Atoi(mem[:len(mem)-1])
		if err != nil {
			return 0, fmt.Errorf("parseMiB: %w", err)
		}
		if n < 1024 {
			return 0, fmt.Errorf("parseMiB: %q is less than 1 MiB", mem)
		}
		return n / 1024, nil
	default:
		n, err := strconv.Atoi(mem)
		if err != nil {
			return 0, fmt.Errorf("parseMiB: %w", err)
		}
		return n, nil
	}
}

// fcSendCtrlAltDel sends a graceful shutdown request to the Firecracker REST API.
func fcSendCtrlAltDel(sockPath string) error {
	dialer := &net.Dialer{}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	body := bytes.NewReader([]byte(`{"action_type":"SendCtrlAltDel"}`))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, "http://localhost/actions", body)
	if err != nil {
		return fmt.Errorf("fc shutdown request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fc shutdown: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// ensureFile creates an empty file at path (and any missing parent dirs) if it
// does not already exist.
func ensureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // daemon-owned runtime log path
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

func fcSocketPath(id string) string {
	return filepath.Join(os.TempDir(), "fc-"+id+".sock")
}

func fcConfigPath(id string) string {
	return filepath.Join(os.TempDir(), "fc-"+id+"-config.json")
}

// Firecracker VM config JSON types.

type fcVMConfig struct {
	BootSource        fcBootSource         `json:"boot-source"`
	Drives            []fcDrive            `json:"drives"`
	MachineConfig     fcMachineConfig      `json:"machine-config"`
	NetworkInterfaces []fcNetworkInterface `json:"network-interfaces,omitempty"`
}

type fcBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type fcDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type fcMachineConfig struct {
	VcpuCount  int  `json:"vcpu_count"`
	MemSizeMib int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}

type fcNetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}
