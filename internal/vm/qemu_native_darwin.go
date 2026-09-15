//go:build darwin && arm64

package vm

import (
	"context"
	"debug/elf"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func DefaultQEMUBinary() string { return "qemu-system-aarch64" }

func validateHostConfig(cfg Config, kernel string) error {
	if cfg.EmulateX86 && cfg.Architecture != "" && cfg.Architecture != "amd64" && cfg.Architecture != "x86_64" {
		return fmt.Errorf("--emulate-x86 requires an x86 image")
	}
	if cfg.Architecture != "arm64" && !cfg.EmulateX86 {
		return fmt.Errorf("native macOS requires an ARM64 image; rebuild with --platform linux/arm64 or explicitly use --emulate-x86 for an x86 image")
	}
	if cfg.CPUShares > 10000 || cfg.MemoryMax < 0 {
		return fmt.Errorf("invalid CPU weight or memory limit")
	}
	if h := cfg.HealthCheck; h != nil {
		if (h.Type != "tcp" && h.Type != "http") || h.Port < 0 || h.Port > 65535 || h.Interval < 0 || h.Timeout < 0 || h.Retries < 0 {
			return fmt.Errorf("invalid native health check")
		}
		if h.Port == 0 && len(cfg.PortMaps) == 0 {
			return fmt.Errorf("health check requires a guest port")
		}
	}
	for _, pm := range cfg.PortMaps {
		if pm.BindAddr != "" && (net.ParseIP(pm.BindAddr) == nil || strings.Contains(pm.BindAddr, ":")) {
			return fmt.Errorf("macOS port bindings require an IPv4 address")
		}
	}
	if cfg.EmulateX86 {
		return nil
	}
	if info, err := os.Stat(kernel); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("ARM64 kernel missing at %s; build kernel PLATFORM=virt and select its tools directory with --tools-dir", kernel)
	}
	k, err := elf.Open(kernel)
	if err != nil {
		return fmt.Errorf("invalid ARM64 boot kernel: %w", err)
	}
	defer k.Close()
	if k.Machine != elf.EM_AARCH64 {
		return fmt.Errorf("native boot requires an ARM64 kernel, got %s", k.Machine)
	}
	return nil
}

// Direct-boot the ARM64 Nanos Image with Apple's Hypervisor.framework.
// HVF failure is fatal: never silently fall back to CPU emulation.
func (m *QEMUManager) buildNativeCmd(ctx context.Context, cfg Config, qmp string) *exec.Cmd {
	drive := "file=" + strings.ReplaceAll(cfg.ImagePath, ",", ",,") + ",format=raw,if=none,id=root,snapshot=on"
	if cfg.DiskIOPS > 0 {
		drive += fmt.Sprintf(",throttling.iops-total=%d", cfg.DiskIOPS)
	}
	if cfg.DiskBPS > 0 {
		drive += fmt.Sprintf(",throttling.bps-total=%d", cfg.DiskBPS)
	}
	args := []string{"-machine", "virt,gic-version=3,highmem=off", "-accel", "hvf", "-cpu", "host",
		"-kernel", m.kernelPath, "-m", cfg.Memory, "-display", "none", "-serial", "stdio", "-monitor", "none", "-no-reboot",
		"-drive", drive, "-device", "virtio-blk-pci,drive=root", "-device", "virtio-rng-pci",
		"-device", "pvpanic-pci", "-action", "panic=exit-failure"}
	binary := m.qemuBin
	if cfg.EmulateX86 {
		binary = "qemu-system-x86_64"
		if filepath.IsAbs(m.qemuBin) {
			binary = filepath.Join(filepath.Dir(m.qemuBin), binary)
		}
		args = []string{"-accel", "tcg,thread=multi", "-cpu", "max", "-m", cfg.Memory, "-display", "none", "-serial", "stdio", "-monitor", "none", "-no-reboot", "-drive", drive, "-device", "virtio-blk-pci,drive=root", "-device", "virtio-rng-pci", "-device", "isa-debug-exit,iobase=0x501,iosize=1"}
	}
	if cfg.CPUs > 0 {
		args = append(args, "-smp", strconv.Itoa(cfg.CPUs))
	}
	netdev := "stream,id=net0,addr.type=unix,addr.path=" + escapeFwCfgValue(cfg.nativeSocket) + ",server=off,reconnect-ms=1000"
	args = append(args, "-netdev", netdev, "-device", "virtio-net-pci,netdev=net0,mac="+cfg.nativeMAC)
	netcfg := cfg
	if netcfg.NetworkName == "" {
		netcfg.IPAddress = "10.0.2.15"
		netcfg.GatewayIP = "10.0.2.2"
		netcfg.SubnetMask = "24"
	}
	args = append(args, buildNetworkCfgArgs(netcfg)...)
	args = append(args, buildEnvArgs(cfg.Env)...)
	args = append(args, buildVolumeArgs(cfg.Volumes)...)
	args = append(args, buildMountArgs(cfg.Volumes)...)
	if qmp != "" {
		args = append(args, "-qmp", qmp+",server,nowait")
	}
	return m.mkCmd(ctx, binary, args...)
}
