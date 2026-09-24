//go:build linux || (darwin && arm64)

package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/stretchr/testify/require"
)

func TestVolumeAPIWithSeparateClientStorage(t *testing.T) {
	store, err := volume.NewStore(t.TempDir())
	require.NoError(t, err)
	s, err := NewServer(vm.NewMockManager(), nil, "tcp://127.0.0.1:0", nil, "test", nil)
	require.NoError(t, err)
	s.SetVolumeStore(store)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()
	c, err := api.Dial("tcp://" + s.listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.VolumeGet(ctx, "missing")
	require.True(t, api.IsNotFound(err))
	v, err := c.VolumeCreate(ctx, "new", 8192)
	require.NoError(t, err)
	require.FileExists(t, v.DiskPath)
	data := []byte("legacy volume bytes")
	v, err = c.VolumeImport(ctx, api.VolumeImportParams{Name: "legacy", Label: "legacy", SizeBytes: int64(len(data))}, bytes.NewReader(data))
	require.NoError(t, err)
	stored, err := c.VolumeGet(ctx, "legacy")
	require.NoError(t, err)
	require.Equal(t, v.DiskPath, stored.DiskPath)
	all, err := c.VolumeList(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.NoError(t, c.VolumeRemove(ctx, "legacy", ""))
	require.NoFileExists(t, v.DiskPath)
}

func TestDaemonOwnedVolume(t *testing.T) {
	root := t.TempDir()
	store, err := volume.NewStore(root)
	require.NoError(t, err)
	mgr := vm.NewMockManager()
	s := &Server{mgr: mgr, volStore: store}
	raw, err := json.Marshal(api.VolumeCreateParams{Name: "data", SizeBytes: 8192})
	require.NoError(t, err)
	result, rpcErr := s.handleVolume("Volume.Create", raw)
	require.Nil(t, rpcErr)
	vol := result.(*volume.Volume)
	require.Equal(t, filepath.Join(root, "data", "disk.img"), vol.DiskPath)
	mounts := []api.VolumeMountSpec{{Name: "data", GuestPath: "/data", DiskPath: "/client/incorrect.img"}}
	require.Nil(t, s.resolveNamedVolumes(mounts))
	require.Equal(t, vol.DiskPath, mounts[0].DiskPath)
	require.Equal(t, "data", mounts[0].Label)
	v, err := mgr.Create(context.Background(), vm.Config{Volumes: []vm.VolumeMount{{DiskPath: vol.DiskPath}}})
	require.NoError(t, err)
	raw = json.RawMessage(`{"name":"data"}`)
	_, rpcErr = s.handleVolumeRemove(raw)
	require.NotNil(t, rpcErr)
	require.Contains(t, rpcErr.Message, "referenced")
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.NoError(t, mgr.Stop(context.Background(), v.ID))
	require.NoError(t, mgr.Remove(context.Background(), v.ID))
	_, rpcErr = s.handleVolumeRemove(raw)
	require.Nil(t, rpcErr)
	require.NoFileExists(t, vol.DiskPath)
	require.NotNil(t, s.resolveNamedVolumes(mounts))
}
