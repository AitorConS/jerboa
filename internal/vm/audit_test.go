//go:build linux

package vm

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditRemovedVMDoesNotResurrect(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = s.Save(v)
		}
	}()
	require.NoError(t, s.Remove(v.ID))
	wg.Wait()
	require.NoError(t, s.Save(v))
	restarted := NewFileStore(dir)
	require.NoError(t, restarted.Restore())
	require.Empty(t, restarted.List())
}
func TestAuditLogicalNetworkName(t *testing.T) {
	require.NoError(t, validateVMConfig(Config{ImagePath: "test.img", Memory: "256M", NetworkName: "audit-long-network-name", BridgeName: "jb-short"}))
}

func TestAuditSQLiteRemovedVMDoesNotResurrect(t *testing.T) {
	s := newSQLiteStore(t)
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = s.Save(v)
		}
	}()
	require.NoError(t, s.Remove(v.ID))
	wg.Wait()
	require.NoError(t, s.Save(v))
	require.NoError(t, s.Restore())
	require.Empty(t, s.List())
	var count int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM vms").Scan(&count))
	require.Zero(t, count)
}
