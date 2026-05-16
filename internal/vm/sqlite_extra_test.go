package vm

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSQLiteStore_RemoveNotFound(t *testing.T) {
	s := newSQLiteStore(t)
	require.NoError(t, s.Restore())
	err := s.Remove("nonexistent")
	require.Error(t, err)
}

func TestSQLiteStore_Save_UpdatesExisting(t *testing.T) {
	s := newSQLiteStore(t)
	require.NoError(t, s.Restore())
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	v.SetHealthStatus(HealthHealthy)
	require.NoError(t, s.Save(v))
	got, err := s.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, HealthHealthy, got.GetHealthStatus())
}

func TestSQLiteStore_Restore_StoppedVM(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s1, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s1.Restore())
	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, v.transition(StateRunning))
	require.NoError(t, v.transition(StateStopping))
	require.NoError(t, v.transition(StateStopped))
	require.NoError(t, s1.Save(v))
	require.NoError(t, s1.Close())
	s2, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s2.Restore())
	t.Cleanup(func() { _ = s2.Close() })
	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, got.GetState())
	require.False(t, got.DaemonRecovered)
}

func TestSQLiteStore_Restore_StartingVM(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s1, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s1.Restore())
	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, s1.Save(v))
	require.NoError(t, s1.Close())
	s2, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s2.Restore())
	t.Cleanup(func() { _ = s2.Close() })
	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, got.GetState())
	require.True(t, got.DaemonRecovered)
}

func TestSQLiteStore_Close(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func TestSQLiteStore_CreateRollbackOnWriteError(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s.Restore())
	require.NoError(t, s.Close())
	_, err = s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.Error(t, err)
}

func TestNullTime_Nil(t *testing.T) {
	result := nullTime(nil)
	require.Nil(t, result)
}

func TestNullTime_Value(t *testing.T) {
	now := time.Now()
	result := nullTime(&now)
	require.NotNil(t, result)
}

func TestBoolToInt_True(t *testing.T) {
	require.Equal(t, 1, boolToInt(true))
}

func TestBoolToInt_False(t *testing.T) {
	require.Equal(t, 0, boolToInt(false))
}

func TestSQLiteStore_Restore_EmptyDB(t *testing.T) {
	s := newSQLiteStore(t)
	require.NoError(t, s.Restore())
	require.NoError(t, s.Restore())
	require.Empty(t, s.List())
}

func TestSQLiteStore_Restore_RestartCount(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s1, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s1.Restore())
	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	v.mu.Lock()
	v.RestartCount = 7
	v.mu.Unlock()
	require.NoError(t, s1.Save(v))
	require.NoError(t, s1.Close())
	s2, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s2.Restore())
	t.Cleanup(func() { _ = s2.Close() })
	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, 7, got.RestartCount)
}

func TestSQLiteStore_Restore_DaemonRecovered(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s1, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s1.Restore())
	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, v.transition(StateRunning))
	require.NoError(t, s1.Save(v))
	require.NoError(t, s1.Close())
	s2, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s2.Restore())
	t.Cleanup(func() { _ = s2.Close() })
	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.True(t, got.DaemonRecovered)
}
