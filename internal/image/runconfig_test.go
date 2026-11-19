package image

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateRunConfig(t *testing.T) {
	// What images carry today: unikernel.toml's grammar, the build default
	// and bare MiB counts from jerboa build --memory.
	for _, mem := range []string{"", "256M", "1G", "512m", "2g", "512Mi", "1Gi", "128", "2048", "1T", "1048576K"} {
		require.NoError(t, ValidateRunConfig(Config{Memory: mem, CPUs: 2}), mem)
	}
	for _, mem := range []string{
		"0", "0M", "-1G", "1G,maxmem=8G", "256M -smp 64", "M", "i", "512i", "1.5G", "2T", "1024T",
		"99999999999999999999M", "512K", " 256M", "256MB", "0256M",
	} {
		require.Error(t, ValidateRunConfig(Config{Memory: mem}), mem)
	}
	require.NoError(t, ValidateRunConfig(Config{CPUs: 0}))
	require.NoError(t, ValidateRunConfig(Config{CPUs: MaxCPUs}))
	require.Error(t, ValidateRunConfig(Config{CPUs: -1}))
	require.Error(t, ValidateRunConfig(Config{CPUs: MaxCPUs + 1}))
	require.NoError(t, ValidateRunConfig(Config{MinDiskSize: 1 << 30}))
	require.Error(t, ValidateRunConfig(Config{MinDiskSize: -1}))
	require.Error(t, ValidateRunConfig(Config{MinDiskSize: MaxMinDiskSize + 1}))
}
