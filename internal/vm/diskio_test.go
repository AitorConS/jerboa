//go:build linux || (darwin && arm64)

package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateDiskIO(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{DiskIOEngine: DiskIOEngineSync, VolumeCache: VolumeCacheWriteback},
		{DiskIOEngine: DiskIOEngineAsync, VolumeCache: VolumeCacheUnsafe},
	} {
		require.NoError(t, validateDiskIO(cfg))
	}
	require.ErrorContains(t, validateDiskIO(Config{DiskIOEngine: "uring"}), "DiskIOEngine")
	require.ErrorContains(t, validateDiskIO(Config{VolumeCache: "none"}), "VolumeCache")
}

func TestFCIOEngine(t *testing.T) {
	require.Equal(t, fcEngineSync, fcIOEngine(Config{DiskIOEngine: DiskIOEngineSync}))
	require.Equal(t, fcEngineAsync, fcIOEngine(Config{DiskIOEngine: DiskIOEngineAsync}))
	require.Equal(t, defaultFCIOEngine(), fcIOEngine(Config{}))
}

// The macOS VMM rejects unknown drive fields, so a drive built without the
// Linux-only options must serialize exactly as before.
func TestFCDriveOmitsOptionalFields(t *testing.T) {
	data, err := json.Marshal(fcDrive{DriveID: "rootfs", PathOnHost: "/d", IsRootDevice: true})
	require.NoError(t, err)
	require.JSONEq(t, `{"drive_id":"rootfs","path_on_host":"/d","is_root_device":true,"is_read_only":false}`, string(data))
}

func TestFCRootRateLimiter(t *testing.T) {
	require.Nil(t, fcRootRateLimiter(Config{}))
	rl := fcRootRateLimiter(Config{DiskIOPS: 500, DiskBPS: 10 << 20})
	require.Equal(t, &fcTokenBucket{Size: 10 << 20, RefillTime: 1000}, rl.Bandwidth)
	require.Equal(t, &fcTokenBucket{Size: 500, RefillTime: 1000}, rl.Ops)
	require.Nil(t, fcRootRateLimiter(Config{DiskIOPS: 1}).Bandwidth)
}

func TestValidateImageLayout(t *testing.T) {
	fc := NewFirecrackerManager("firecracker", "vmlinux")
	require.NoError(t, fc.ValidateImageLayout("compact", false))

	q := NewQEMUManager("qemu")
	require.NoError(t, q.ValidateImageLayout("", false))
	require.NoError(t, q.ValidateImageLayout("", true))
	if runtime.GOOS == "darwin" {
		require.NoError(t, q.ValidateImageLayout("compact", false), "native ARM64 loads the kernel directly")
		require.ErrorContains(t, q.ValidateImageLayout("compact", true), "BIOS", "x86 emulation cannot boot PVH under TCG")
		return
	}

	// Linux: a compact image needs KVM and the x86 PVH kernel.
	tools := t.TempDir()
	kernel := filepath.Join(tools, "kernel.img")
	require.NoError(t, os.WriteFile(kernel, []byte("kernel"), 0o644))
	withKernel := NewQEMUManager("qemu", WithKernel(kernel))
	missing := NewQEMUManager("qemu", WithKernel(filepath.Join(tools, "missing.img")))
	if len(kvmAccelArgs()) == 0 {
		require.ErrorContains(t, withKernel.ValidateImageLayout("compact", false), "KVM")
		return
	}
	require.NoError(t, withKernel.ValidateImageLayout("compact", false))
	require.ErrorContains(t, missing.ValidateImageLayout("compact", false), "x86 kernel")
	require.ErrorContains(t, q.ValidateImageLayout("compact", false), "tools directory")
}
