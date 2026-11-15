//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"syscall"
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

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, want, info.Mode().Perm(), path)
}

// vms.db holds every VM's config, Env included; it and its directories must
// be owner-only whatever the umask, as the file backend's state.json is.
func TestSQLiteStore_OwnerOnly(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	home := t.TempDir()
	dir := filepath.Join(home, ".jerboa", "vms")
	dbPath := filepath.Join(dir, "vms.db")
	s, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.Restore())
	requireMode(t, filepath.Join(home, ".jerboa"), 0o700)
	requireMode(t, dir, 0o700)
	requireMode(t, dbPath, 0o600)

	// Journal files SQLite creates later take the database file's mode.
	_, err = s.db.Exec("PRAGMA journal_mode=WAL")
	require.NoError(t, err)
	_, err = s.Create(Config{ImagePath: "test.img", Memory: "256M", Env: []string{"TOKEN=secret"}})
	require.NoError(t, err)
	requireMode(t, dbPath+"-wal", 0o600)
	requireMode(t, dbPath+"-shm", 0o600)
}

// A store left world-readable by an earlier version is tightened on open.
func TestSQLiteStore_TightensExistingFiles(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := filepath.Join(t.TempDir(), "vms")
	require.NoError(t, os.Mkdir(dir, 0o755))
	dbPath := filepath.Join(dir, "vms.db")
	s, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, os.Chmod(dir, 0o755))
	require.NoError(t, os.Chmod(dbPath, 0o644))

	s, err = NewSQLiteStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.Restore())
	requireMode(t, dir, 0o700)
	requireMode(t, dbPath, 0o600)

	// Journal files left behind are tightened too.
	other := filepath.Join(t.TempDir(), "other.db")
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		require.NoError(t, os.WriteFile(other+suffix, nil, 0o644))
	}
	require.NoError(t, restrictSQLiteFiles(other))
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		requireMode(t, other+suffix, 0o600)
	}
}
