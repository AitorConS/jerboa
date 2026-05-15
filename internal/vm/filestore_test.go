package vm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFileStore_CreateAndGet(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())

	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.Equal(t, StateCreated, v.GetState())

	got, err := s.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
}

func TestFileStore_PersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())

	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M", Name: "myvm"})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, v.ID, "state.json"))
	require.NoError(t, err)
	require.Contains(t, string(data), v.ID)
	require.Contains(t, string(data), "myvm")
}

func TestFileStore_Restore(t *testing.T) {
	dir := t.TempDir()
	s1 := NewFileStore(dir)
	require.NoError(t, s1.Restore())

	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M", Name: "restore-vm"})
	require.NoError(t, err)

	s2 := NewFileStore(dir)
	require.NoError(t, s2.Restore())

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
	require.Equal(t, "restore-vm", got.Cfg.Name)
}

func TestFileStore_Restore_RunningVMStopped(t *testing.T) {
	dir := t.TempDir()
	s1 := NewFileStore(dir)
	require.NoError(t, s1.Restore())

	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, v.transition(StateRunning))
	require.NoError(t, s1.Save(v))

	s2 := NewFileStore(dir)
	require.NoError(t, s2.Restore())

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, got.GetState())
	require.True(t, got.DaemonRecovered)
}

func TestFileStore_Restore_CreatedVM(t *testing.T) {
	dir := t.TempDir()
	s1 := NewFileStore(dir)
	require.NoError(t, s1.Restore())

	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M", Name: "created-vm"})
	require.NoError(t, err)

	s2 := NewFileStore(dir)
	require.NoError(t, s2.Restore())

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateCreated, got.GetState())
	require.False(t, got.DaemonRecovered)
}

func TestFileStore_Restore_StoppedVM(t *testing.T) {
	dir := t.TempDir()
	s1 := NewFileStore(dir)
	require.NoError(t, s1.Restore())

	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, v.transition(StateRunning))
	require.NoError(t, v.transition(StateStopping))
	require.NoError(t, v.transition(StateStopped))
	require.NoError(t, s1.Save(v))

	s2 := NewFileStore(dir)
	require.NoError(t, s2.Restore())

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, got.GetState())
	require.False(t, got.DaemonRecovered)
}

func TestFileStore_Remove(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())

	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	require.NoError(t, s.Remove(v.ID))
	_, err = s.Get(v.ID)
	require.Error(t, err)

	_, err = os.Stat(filepath.Join(dir, v.ID))
	require.True(t, os.IsNotExist(err))
}

func TestFileStore_ResolveByName(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())

	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M", Name: "myapp"})
	require.NoError(t, err)

	got, err := s.Resolve("myapp")
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
}

func TestFileStore_List(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())

	_, _ = s.Create(Config{ImagePath: "a.img", Memory: "256M"})
	_, _ = s.Create(Config{ImagePath: "b.img", Memory: "256M"})
	list := s.List()
	require.Len(t, list, 2)
}

func TestFileStore_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())
	list := s.List()
	require.Empty(t, list)
}

func TestFileStore_NonexistentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())
	list := s.List()
	require.Empty(t, list)
}

func TestFileStore_SaveUpdatesState(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())

	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, v.transition(StateRunning))
	require.NoError(t, s.Save(v))

	s2 := NewFileStore(dir)
	require.NoError(t, s2.Restore())

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, got.GetState())
	require.True(t, got.DaemonRecovered)
}

func TestFileStore_StartAtTimestamp(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())

	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	now := time.Now()
	v.mu.Lock()
	v.StartedAt = &now
	v.mu.Unlock()
	require.NoError(t, v.transition(StateRunning))
	require.NoError(t, s.Save(v))

	s2 := NewFileStore(dir)
	require.NoError(t, s2.Restore())

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.NotNil(t, got.StartedAt)
}

func TestFileStore_Restore_HealthStatus(t *testing.T) {
	dir := t.TempDir()
	s1 := NewFileStore(dir)
	require.NoError(t, s1.Restore())

	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	v.SetHealthStatus(HealthHealthy)
	require.NoError(t, s1.Save(v))

	s2 := NewFileStore(dir)
	require.NoError(t, s2.Restore())

	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, HealthHealthy, got.GetHealthStatus())
}
