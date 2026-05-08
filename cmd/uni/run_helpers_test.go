package main

import (
	"path/filepath"
	"testing"

	"github.com/AitorConS/unikernel-engine/internal/volume"
	"github.com/stretchr/testify/require"
)

func TestResolveVolumes(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "images")
	volRoot := volumeStorePath(storePath)
	store, err := volume.NewStore(volRoot)
	require.NoError(t, err)

	_, err = store.Create("data", 8*1024*1024)
	require.NoError(t, err)

	vols, err := resolveVolumes([]string{"data:/mnt/data:ro"}, storePath)
	require.NoError(t, err)
	require.Len(t, vols, 1)
	require.Equal(t, "/mnt/data", vols[0].GuestPath)
	require.True(t, vols[0].ReadOnly)

	_, err = resolveVolumes([]string{"missing:/mnt"}, storePath)
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
