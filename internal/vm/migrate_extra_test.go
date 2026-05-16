package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrator_SkipsFilesInRoot(t *testing.T) {
	fileDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(fileDir, "random.txt"), []byte("data"), 0o644))
	sqliteStore := newSQLiteStore(t)
	require.NoError(t, sqliteStore.Restore())
	m := NewMigrator(fileDir, sqliteStore)
	n, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestMigrator_SkipsMissingStateJSON(t *testing.T) {
	fileDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(fileDir, "emptyvm"), 0o755))
	sqliteStore := newSQLiteStore(t)
	require.NoError(t, sqliteStore.Restore())
	m := NewMigrator(fileDir, sqliteStore)
	n, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestMigrator_StoppedVMPreserved(t *testing.T) {
	fileDir := t.TempDir()
	fs := NewFileStore(fileDir)
	require.NoError(t, fs.Restore())
	v, err := fs.Create(Config{ImagePath: "stop.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, v.transition(StateRunning))
	require.NoError(t, v.transition(StateStopping))
	require.NoError(t, v.transition(StateStopped))
	require.NoError(t, fs.Save(v))

	sqliteStore := newSQLiteStore(t)
	require.NoError(t, sqliteStore.Restore())
	m := NewMigrator(fileDir, sqliteStore)
	n, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got, err := sqliteStore.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, got.GetState())
	require.False(t, got.DaemonRecovered)
}

func TestMigrator_CreatedVMPreserved(t *testing.T) {
	fileDir := t.TempDir()
	fs := NewFileStore(fileDir)
	require.NoError(t, fs.Restore())
	_, err := fs.Create(Config{ImagePath: "created.img", Memory: "256M", Name: "created-vm"})
	require.NoError(t, err)

	sqliteStore := newSQLiteStore(t)
	require.NoError(t, sqliteStore.Restore())
	m := NewMigrator(fileDir, sqliteStore)
	n, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, sqliteStore.List(), 1)
}

func TestMigrator_DaemonRecoveredPreserved(t *testing.T) {
	fileDir := t.TempDir()
	fs := NewFileStore(fileDir)
	require.NoError(t, fs.Restore())
	v, err := fs.Create(Config{ImagePath: "dr.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, v.transition(StateRunning))
	require.NoError(t, fs.Save(v))

	s2 := NewFileStore(fileDir)
	require.NoError(t, s2.Restore())
	got, _ := s2.Get(v.ID)
	require.True(t, got.DaemonRecovered)

	sqliteStore := newSQLiteStore(t)
	require.NoError(t, sqliteStore.Restore())
	m := NewMigrator(fileDir, sqliteStore)
	n, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got2, err := sqliteStore.Get(v.ID)
	require.NoError(t, err)
	require.True(t, got2.DaemonRecovered)
}
