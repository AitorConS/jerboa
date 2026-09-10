package vm

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeQEMUUsesHVFAndPrivateNetworkStream(t *testing.T) {
	m := NewQEMUManager(DefaultQEMUBinary(), WithKernel("/tools/kernel.img"))
	cfg := Config{ImagePath: "/images/a,b.img", Memory: "256M", CPUs: 2, nativeSocket: "/tmp/net.sock", nativeMAC: "02:01:02:03:04:05",
		PortMaps: []PortMap{{HostPort: 8080, GuestPort: 80, BindAddr: "127.0.0.1"}}}
	args := strings.Join(m.buildCmd(context.Background(), cfg, "unix:/tmp/qmp.sock").Args, " ")
	require.Contains(t, args, "-accel hvf -cpu host")
	require.Contains(t, args, "-kernel /tools/kernel.img")
	require.Contains(t, args, "file=/images/a,,b.img")
	require.Contains(t, args, "addr.path=/tmp/net.sock,server=off,reconnect-ms=1000")
	require.Contains(t, args, "-device pvpanic-pci -action panic=exit-failure")
	for _, forbidden := range []string{"tcg", "enable-kvm", "isa-debug-exit", "tap,id="} {
		require.NotContains(t, args, forbidden)
	}
}

func TestNativeValidatesArchitectureAndLimits(t *testing.T) {
	kernel := filepath.Join(t.TempDir(), "kernel.img")
	header := make([]byte, 64)
	copy(header, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(header[16:], 2)
	binary.LittleEndian.PutUint16(header[18:], 183)
	binary.LittleEndian.PutUint32(header[20:], 1)
	binary.LittleEndian.PutUint16(header[52:], 64)
	require.NoError(t, os.WriteFile(kernel, header, 0600))
	require.NoError(t, validateHostConfig(Config{Architecture: "arm64", CPUShares: 100, MemoryMax: 512 << 20, Restart: RestartConfig{Policy: RestartOnFailure}, HealthCheck: &HealthCheckConfig{Type: "tcp", Port: 8080}}, kernel))
	require.Error(t, validateHostConfig(Config{Architecture: "amd64"}, kernel))
	require.NoError(t, validateHostConfig(Config{Architecture: "amd64", EmulateX86: true}, ""))
	require.Error(t, validateHostConfig(Config{Architecture: "arm64", EmulateX86: true}, kernel))
	require.Error(t, validateHostConfig(Config{Architecture: "arm64", CPUShares: 10001}, kernel))
	require.Error(t, validateHostConfig(Config{Architecture: "arm64"}, "/missing"))
}

func TestNativeX86CompatibilityIsExplicit(t *testing.T) {
	m := NewQEMUManager("/opt/homebrew/bin/qemu-system-aarch64", WithKernel("/tools/kernel.img"))
	cfg := Config{EmulateX86: true, Architecture: "amd64", Memory: "256M", ImagePath: "/image", nativeSocket: "/tmp/net.sock", nativeMAC: "02:01:02:03:04:05"}
	cmd := m.buildCmd(context.Background(), cfg, "")
	require.Equal(t, "/opt/homebrew/bin/qemu-system-x86_64", cmd.Args[0])
	args := strings.Join(cmd.Args, " ")
	require.Contains(t, args, "-accel tcg,thread=multi")
	require.Contains(t, args, "isa-debug-exit")
	require.NotContains(t, args, "-kernel")
	require.NotContains(t, args, "-accel hvf")
}

func TestNativeStatsReadActualProcess(t *testing.T) {
	v := &VM{ID: "test", State: StateRunning}
	stats := (&darwinStats{pid: os.Getpid(), vm: v}).Collect()
	require.Equal(t, darwinStatsSource, stats.Source)
	require.Greater(t, stats.MemBytes, int64(0))
}
