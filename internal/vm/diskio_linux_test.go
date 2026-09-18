//go:build linux

package vm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKernelAtLeast(t *testing.T) {
	for release, want := range map[string]bool{
		"6.8.0-45-generic":                   true,
		"5.10.0":                             true,
		"5.15.153.1-microsoft-standard-WSL2": true,
		"5.4.0-150-generic":                  false,
		"4.19.0":                             false,
		"5.10rc1":                            true,
		"garbage":                            false,
	} {
		require.Equal(t, want, kernelAtLeast(release, 5, 10), release)
	}
}

func TestValidateDiskIOHostSyncAlwaysAllowed(t *testing.T) {
	require.NoError(t, validateDiskIOHost(Config{}))
	require.NoError(t, validateDiskIOHost(Config{DiskIOEngine: DiskIOEngineSync}))
}
