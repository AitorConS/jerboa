package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveVolumes(t *testing.T) {
	vols, err := resolveVolumes([]string{"data:/mnt/data:ro"})
	require.NoError(t, err)
	require.Len(t, vols, 1)
	require.Equal(t, "data", vols[0].Name)
	require.Equal(t, "/mnt/data", vols[0].GuestPath)
	require.True(t, vols[0].ReadOnly)
	require.Empty(t, vols[0].DiskPath)
	_, err = resolveVolumes([]string{"data:relative"})
	require.Error(t, err)
}

func TestParseVolumePortString(t *testing.T) {
	pm, err := parseVolumePortString("8080:80/tcp")
	require.NoError(t, err)
	require.Equal(t, uint16(8080), pm.HostPort)
	require.Equal(t, uint16(80), pm.GuestPort)

	_, err = parseVolumePortString("bad")
	require.Error(t, err)
}

func TestExtractMask(t *testing.T) {
	require.Equal(t, "26", extractMask("10.100.0.0/26"))
	require.Equal(t, "24", extractMask("10.100.0.0"))
}

func TestParseMemoryMax(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"512M", 512 * 1024 * 1024},
		{"1G", 1024 * 1024 * 1024},
		{"256K", 256 * 1024},
		{"1024", 1024},
		{"2g", 2 * 1024 * 1024 * 1024},
		{"", 0},
		{"10MB", 10 * 1024 * 1024},
		{"10MiB", 10 * 1024 * 1024},
		{"2GB", 2 * 1024 * 1024 * 1024},
		{"512b", 512},
	}
	for _, tc := range tests {
		got, err := parseByteSize("memory-max", tc.input)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	_, err := parseByteSize("memory-max", "-1M")
	require.Error(t, err)
	_, err = parseByteSize("disk-bps", "abc")
	require.ErrorContains(t, err, "disk-bps", "the error must name the flag being parsed")
}
