//go:build linux

package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPrepareBootDiskIndependentCopy verifies prepareBootDisk produces a
// byte-identical but independent file: writing to the destination must not
// affect the source. This is the property that lets several Firecracker VMs
// share a base image safely.
func TestPrepareBootDiskIndependentCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "base.img")
	dst := filepath.Join(dir, "vm-rootfs.img")
	want := []byte("unikernel base image contents")
	require.NoError(t, os.WriteFile(src, want, 0o600))

	require.NoError(t, prepareBootDisk(dst, src, ""))

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, want, got)

	// Mutating the copy must leave the base pristine.
	require.NoError(t, os.WriteFile(dst, []byte("guest writes"), 0o600))
	base, err := os.ReadFile(src)
	require.NoError(t, err)
	require.Equal(t, want, base, "base image must not change when the copy is written")
}

// TestWriteFCConfigUsesRootfsCopy verifies the generated Firecracker config
// points the root drive at the per-VM copy, not the shared store image, so
// concurrent VMs never write to the same backing file.
func TestWriteFCConfigUsesRootfsCopy(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "vmlinux")
	cfg := Config{ImagePath: "/store/images/app.img", Memory: "256M", CPUs: 1}
	rootfs := filepath.Join(t.TempDir(), "fc-test-rootfs.img")

	cfgPath, err := m.writeFCConfig("test-vm", cfg, rootfs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(cfgPath) })

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	var parsed fcVMConfig
	require.NoError(t, json.Unmarshal(data, &parsed))

	require.NotEmpty(t, parsed.Drives)
	root := parsed.Drives[0]
	require.Equal(t, "rootfs", root.DriveID)
	require.True(t, root.IsRootDevice)
	require.Equal(t, rootfs, root.PathOnHost, "root drive must use the per-VM copy")
	require.NotEqual(t, cfg.ImagePath, root.PathOnHost, "root drive must not use the shared store image")
}

func writeParsedFCConfig(t *testing.T, cfg Config) fcVMConfig {
	t.Helper()
	m := NewFirecrackerManager("firecracker", "vmlinux")
	cfgPath, err := m.writeFCConfig("test-vm", cfg, filepath.Join(t.TempDir(), "rootfs.img"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(cfgPath) })
	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	var parsed fcVMConfig
	require.NoError(t, json.Unmarshal(data, &parsed))
	return parsed
}

// TestWriteFCConfigDriveDurability verifies volumes forward guest flushes by
// default (Firecracker's own default, Unsafe, silently drops them), while the
// disposable root clone keeps the faster Unsafe mode.
func TestWriteFCConfigDriveDurability(t *testing.T) {
	cfg := Config{ImagePath: "/store/app.img", Memory: "256M", CPUs: 1,
		Volumes: []VolumeMount{{DiskPath: "/vol/pg.img", GuestPath: "/data"}}}
	parsed := writeParsedFCConfig(t, cfg)
	require.Len(t, parsed.Drives, 2)
	require.Equal(t, "Unsafe", parsed.Drives[0].CacheType)
	require.Equal(t, "Writeback", parsed.Drives[1].CacheType)
	for _, d := range parsed.Drives {
		require.Equal(t, "Sync", d.IoEngine)
	}
	require.Nil(t, parsed.Drives[0].RateLimiter)

	cfg.VolumeCache = VolumeCacheUnsafe
	cfg.DiskIOEngine = DiskIOEngineAsync
	cfg.DiskIOPS = 1000
	parsed = writeParsedFCConfig(t, cfg)
	require.Equal(t, "Unsafe", parsed.Drives[1].CacheType)
	for _, d := range parsed.Drives {
		require.Equal(t, "Async", d.IoEngine)
	}
	require.NotNil(t, parsed.Drives[0].RateLimiter)
	require.Equal(t, int64(1000), parsed.Drives[0].RateLimiter.Ops.Size)
	require.Nil(t, parsed.Drives[1].RateLimiter, "throttling applies to the boot disk only, like QEMU")
}

func TestWriteFCConfigMetricsDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "metrics")
	m := NewFirecrackerManager("firecracker", "vmlinux", WithFCMetricsDir(dir))
	cfgPath, err := m.writeFCConfig("vm1", Config{ImagePath: "/i.img", Memory: "64M"}, "/rootfs.img")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(cfgPath) })
	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	var parsed fcVMConfig
	require.NoError(t, json.Unmarshal(data, &parsed))
	require.NotNil(t, parsed.Metrics)
	require.Equal(t, filepath.Join(dir, "fc-vm1-metrics.json"), parsed.Metrics.MetricsPath)
	require.FileExists(t, parsed.Metrics.MetricsPath)

	parsed = writeParsedFCConfig(t, Config{ImagePath: "/i.img", Memory: "64M"})
	require.Nil(t, parsed.Metrics)
}
