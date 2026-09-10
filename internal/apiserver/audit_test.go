//go:build linux

package apiserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/stretchr/testify/require"
)

func TestAuditVolumeRemoveRejectsReferencedDisk(t *testing.T) {
	s, mgr, _ := newBugServer(t)
	store, err := volume.NewStore(t.TempDir())
	require.NoError(t, err)
	vol, err := store.Create("data", 4096)
	require.NoError(t, err)
	v, err := mgr.Create(context.Background(), vm.Config{ImagePath: "test.img", Memory: "256M", Volumes: []vm.VolumeMount{{DiskPath: vol.DiskPath}}})
	require.NoError(t, err)
	raw, err := json.Marshal(api.VolumeRemoveParams{Name: "data", DiskPath: vol.DiskPath})
	require.NoError(t, err)
	_, rpcErr := s.handleVolumeRemove(raw)
	require.NotNil(t, rpcErr)
	require.Contains(t, rpcErr.Message, "referenced")
	require.FileExists(t, vol.DiskPath)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.NoError(t, mgr.Stop(context.Background(), v.ID))
	require.NoError(t, mgr.Remove(context.Background(), v.ID))
	_, rpcErr = s.handleVolumeRemove(raw)
	require.Nil(t, rpcErr)
	require.NoFileExists(t, vol.DiskPath)
}
