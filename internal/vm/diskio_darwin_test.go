//go:build darwin && arm64

package vm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDarwinRejectsLinuxOnlyDiskIO(t *testing.T) {
	cfg := Config{Architecture: "arm64", ImagePath: "x", Memory: "64M"}
	async := cfg
	async.DiskIOEngine = DiskIOEngineAsync
	require.ErrorContains(t, validateHostConfig(async, ""), "io_uring")

	m := NewFirecrackerManager("firecracker", "kernel.img")
	unsafe := cfg
	unsafe.VolumeCache = VolumeCacheUnsafe
	require.ErrorContains(t, m.validateFCPlatform(unsafe), "volume-cache unsafe")
}

func TestDarwinDefaultFCIOEngineIsSync(t *testing.T) {
	require.Equal(t, fcEngineSync, fcIOEngine(Config{}))
}

func TestDarwinNeverSetsHugePages(t *testing.T) {
	require.Empty(t, fcHugePages(1024))
}
