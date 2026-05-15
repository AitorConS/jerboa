package vm

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLiteStore_CreateAndGet(t *testing.T) {
	s := newSQLiteStore(t)
	require.NoError(t, s.Restore())

	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.Equal(t, StateCreated, v.GetState())

	got, err := s.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
}

func TestSQLiteStore_CreateWritesToDB(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.Restore())

	_, err = s.Create(Config{ImagePath: "test.img", Memory: "256M", Name: "dbvm"})
	require.NoError(t, err)

	var count int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM vms").Scan(&count))
	require.Equal(t, 1, count)
}

func TestSQLiteStore_Restore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s1, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s1.Restore())
	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M", Name: "restore-vm"})
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s2.Restore())
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
	require.Equal(t, "restore-vm", got.Cfg.Name)
}

func TestSQLiteStore_Restore_RunningVMStopped(t *testing.T) {
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
	require.Equal(t, StateStopped, got.GetState())
	require.True(t, got.DaemonRecovered)
}

func TestSQLiteStore_Restore_CreatedVM(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s1, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s1.Restore())
	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M", Name: "created-vm"})
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s2.Restore())
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateCreated, got.GetState())
	require.False(t, got.DaemonRecovered)
}

func TestSQLiteStore_Remove(t *testing.T) {
	s := newSQLiteStore(t)
	require.NoError(t, s.Restore())

	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, s.Remove(v.ID))
	_, err = s.Get(v.ID)
	require.Error(t, err)

	var count int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM vms").Scan(&count))
	require.Equal(t, 0, count)
}

func TestSQLiteStore_ResolveByName(t *testing.T) {
	s := newSQLiteStore(t)
	require.NoError(t, s.Restore())

	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M", Name: "myapp"})
	require.NoError(t, err)

	got, err := s.Resolve("myapp")
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
}

func TestSQLiteStore_List(t *testing.T) {
	s := newSQLiteStore(t)
	require.NoError(t, s.Restore())

	_, _ = s.Create(Config{ImagePath: "a.img", Memory: "256M"})
	_, _ = s.Create(Config{ImagePath: "b.img", Memory: "256M"})
	list := s.List()
	require.Len(t, list, 2)
}

func TestSQLiteStore_EmptyDB(t *testing.T) {
	s := newSQLiteStore(t)
	require.NoError(t, s.Restore())
	list := s.List()
	require.Empty(t, list)
}

func TestSQLiteStore_SaveUpdatesState(t *testing.T) {
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
	require.Equal(t, StateStopped, got.GetState())
	require.True(t, got.DaemonRecovered)
}

func TestSQLiteStore_StartAtTimestamp(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s1, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s1.Restore())
	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	now := time.Now()
	v.mu.Lock()
	v.StartedAt = &now
	v.mu.Unlock()
	require.NoError(t, v.transition(StateRunning))
	require.NoError(t, s1.Save(v))
	require.NoError(t, s1.Close())

	s2, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s2.Restore())
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.NotNil(t, got.StartedAt)
}

func TestSQLiteStore_NonexistentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "dir")
	s, err := NewSQLiteStore(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.Restore())
	list := s.List()
	require.Empty(t, list)
}

func TestSQLiteStore_RestoreIdempotent(t *testing.T) {
	s := newSQLiteStore(t)
	require.NoError(t, s.Restore())

	_, _ = s.Create(Config{ImagePath: "test.img", Memory: "256M", Name: "idempotent"})

	require.NoError(t, s.Restore())
	list := s.List()
	require.Len(t, list, 1)
}

func TestSQLiteStore_Restore_HealthStatus(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s1, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s1.Restore())
	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	v.SetHealthStatus(HealthUnhealthy)
	require.NoError(t, s1.Save(v))
	require.NoError(t, s1.Close())

	s2, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s2.Restore())
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, HealthUnhealthy, got.GetHealthStatus())
}

func TestSQLiteStore_CreateFailsOnWriteError(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, s.Restore())
	require.NoError(t, s.Close())

	_, err = s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.Error(t, err)
}
