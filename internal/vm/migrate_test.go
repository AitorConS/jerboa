package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrator_EmptyDir(t *testing.T) {
	sqliteStore := newSQLiteStore(t)
	require.NoError(t, sqliteStore.Restore())

	m := NewMigrator(t.TempDir(), sqliteStore)
	n, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestMigrator_NoFileStoreDir(t *testing.T) {
	sqliteStore := newSQLiteStore(t)
	require.NoError(t, sqliteStore.Restore())

	m := NewMigrator(filepath.Join(t.TempDir(), "nonexistent"), sqliteStore)
	n, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestMigrator_MigratesVms(t *testing.T) {
	fileDir := t.TempDir()
	fs := NewFileStore(fileDir)
	require.NoError(t, fs.Restore())

	v1, err := fs.Create(Config{ImagePath: "a.img", Memory: "256M", Name: "migrate1"})
	require.NoError(t, err)
	v2, err := fs.Create(Config{ImagePath: "b.img", Memory: "512M", Name: "migrate2"})
	require.NoError(t, err)

	sqliteStore := newSQLiteStore(t)
	require.NoError(t, sqliteStore.Restore())

	m := NewMigrator(fileDir, sqliteStore)
	n, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 2, n)

	got1, err := sqliteStore.Get(v1.ID)
	require.NoError(t, err)
	require.Equal(t, "migrate1", got1.Cfg.Name)

	got2, err := sqliteStore.Get(v2.ID)
	require.NoError(t, err)
	require.Equal(t, "migrate2", got2.Cfg.Name)
}

func TestMigrator_Idempotent(t *testing.T) {
	fileDir := t.TempDir()
	fs := NewFileStore(fileDir)
	require.NoError(t, fs.Restore())

	_, err := fs.Create(Config{ImagePath: "a.img", Memory: "256M", Name: "idem-vm"})
	require.NoError(t, err)

	sqliteStore := newSQLiteStore(t)
	require.NoError(t, sqliteStore.Restore())

	m := NewMigrator(fileDir, sqliteStore)
	n1, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 1, n1)

	n2, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 0, n2)

	require.Len(t, sqliteStore.List(), 1)
}

func TestMigrator_RunningVMBecomesStopped(t *testing.T) {
	fileDir := t.TempDir()
	fs := NewFileStore(fileDir)
	require.NoError(t, fs.Restore())

	v, err := fs.Create(Config{ImagePath: "run.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, v.transition(StateRunning))
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
	require.True(t, got.DaemonRecovered)
}

func TestMigrator_SkipsCorruptedFile(t *testing.T) {
	fileDir := t.TempDir()
	fs := NewFileStore(fileDir)
	require.NoError(t, fs.Restore())

	v, err := fs.Create(Config{ImagePath: "good.img", Memory: "256M", Name: "good-vm"})
	require.NoError(t, err)

	badDir := filepath.Join(fileDir, "badvm")
	require.NoError(t, os.MkdirAll(badDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(badDir, "state.json"), []byte("not json"), 0o644))

	sqliteStore := newSQLiteStore(t)
	require.NoError(t, sqliteStore.Restore())

	m := NewMigrator(fileDir, sqliteStore)
	n, err := m.Migrate()
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got, err := sqliteStore.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, "good-vm", got.Cfg.Name)
}