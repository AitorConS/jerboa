//go:build linux || (darwin && arm64)

package vm

import (
	"encoding/json"
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
	require.Error(t, q.ValidateImageLayout("compact", true), "x86 emulation boots through BIOS")
	if runtime.GOOS == "darwin" {
		require.NoError(t, q.ValidateImageLayout("compact", false), "native ARM64 loads the kernel directly")
	} else {
		require.ErrorContains(t, q.ValidateImageLayout("compact", false), "BIOS")
	}
}
