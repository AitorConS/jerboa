//go:build linux || (darwin && arm64)

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The move from the file registry to SQLite after things go wrong: records a
// power cut damaged, a daemon killed half-way through, a migration that runs
// again at every start. No VM may be lost, duplicated or rolled back to an
// older state, and one unreadable record must not stop the rest.

// fileRegistry writes a file registry holding one VM per name and returns the
// directory and the VMs.
func fileRegistry(t *testing.T, names ...string) (string, []*VM) {
	t.Helper()
	dir := t.TempDir()
	fs := NewFileStore(dir)
	var vms []*VM
	for _, name := range names {
		v, err := fs.Create(Config{Name: name, ImagePath: name + ".img", Memory: "256M"})
		require.NoError(t, err)
		vms = append(vms, v)
	}
	return dir, vms
}

// startDaemonStore opens the SQLite registry the way the daemon does: open,
// migrate the file registry found beside it, then restore.
func startDaemonStore(t *testing.T, dir string) (*SQLiteStore, int) {
	t.Helper()
	s := openSQLite(t, filepath.Join(dir, "vms.db"))
	n, err := NewMigrator(dir, s).Migrate()
	require.NoError(t, err)
	require.NoError(t, s.Restore())
	return s, n
}

func TestFaults_MigrationIsDurable(t *testing.T) {
	dir, vms := fileRegistry(t, "a", "b")
	s, n := startDaemonStore(t, dir)
	require.Equal(t, 2, n)
	require.NoError(t, s.Close())

	// The file registry is gone (the operator cleaned up after upgrading):
	// the migrated VMs must now live in the database.
	for _, v := range vms {
		require.NoError(t, os.RemoveAll(filepath.Join(dir, v.ID)))
	}
	again := restoredSQLite(t, filepath.Join(dir, "vms.db"))
	assert.ElementsMatch(t, []string{vms[0].ID, vms[1].ID}, sqliteIDs(again),
		"migrated VMs were only held in memory and are lost with the file registry")
}

func TestFaults_MigrationDoesNotRollBackNewerState(t *testing.T) {
	dir, vms := fileRegistry(t, "app")
	s, _ := startDaemonStore(t, dir)
	v, err := s.Get(vms[0].ID)
	require.NoError(t, err)
	// The VM lives on under SQLite: its state moves ahead of the old file.
	v.mu.Lock()
	v.RestartCount = 7
	v.State = StateStopped
	v.mu.Unlock()
	require.NoError(t, s.Save(v))
	require.NoError(t, s.Close())

	// Next daemon start: the stale state.json is still there and is migrated
	// again. The database must win.
	again, n := startDaemonStore(t, dir)
	assert.Zero(t, n, "a VM already in the database was migrated again")
	got, err := again.Get(vms[0].ID)
	require.NoError(t, err)
	assert.Equal(t, 7, got.GetRestartCount(), "the stale file record overwrote the database")
	assert.Equal(t, StateStopped, got.GetState())
	assert.Len(t, again.List(), 1)
}

func TestFaults_MigrationSkipsDamagedRecords(t *testing.T) {
	damage := map[string]func(t *testing.T, path string){
		"deleted": func(t *testing.T, p string) { require.NoError(t, os.Remove(p)) },
		"empty":   func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, nil, 0o600)) },
		"truncated": func(t *testing.T, p string) {
			require.NoError(t, os.WriteFile(p, []byte(`{"id":"abc","config":{"na`), 0o600))
		},
		"garbage": func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, []byte{0x00, 0xff, 0x10}, 0o600)) },
		"zeroes":  func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, make([]byte, 512), 0o600)) },
		"wrong type": func(t *testing.T, p string) {
			require.NoError(t, os.WriteFile(p, []byte(`{"id":7,"config":"nope"}`), 0o600))
		},
		"is a directory": func(t *testing.T, p string) {
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Mkdir(p, 0o700))
		},
	}
	for name, fn := range damage {
		t.Run(name, func(t *testing.T) {
			dir, vms := fileRegistry(t, "broken", "fine")
			fn(t, filepath.Join(dir, vms[0].ID, "state.json"))
			// Things that are not VM records at all.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o600))
			require.NoError(t, os.Mkdir(filepath.Join(dir, "empty-dir"), 0o700))

			s, n := startDaemonStore(t, dir)
			assert.Equal(t, 1, n)
			assert.Equal(t, []string{vms[1].ID}, sqliteIDs(s))
			require.NoError(t, s.Close())
			assert.Equal(t, []string{vms[1].ID}, sqliteIDs(restoredSQLite(t, filepath.Join(dir, "vms.db"))))
		})
	}
}

func TestFaults_MigrationInterruptedHalfWayCompletesOnTheNextStart(t *testing.T) {
	dir, vms := fileRegistry(t, "a", "b", "c")
	dbPath := filepath.Join(dir, "vms.db")

	// First start: the daemon is killed after migrating only some records.
	// Reproduce it by hiding two of them during the first run.
	hidden := filepath.Join(t.TempDir(), "hidden")
	require.NoError(t, os.Mkdir(hidden, 0o700))
	for _, v := range vms[1:] {
		require.NoError(t, os.Rename(filepath.Join(dir, v.ID), filepath.Join(hidden, v.ID)))
	}
	first := openSQLite(t, dbPath)
	n, err := NewMigrator(dir, first).Migrate()
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, first.Close()) // killed before Restore
	for _, v := range vms[1:] {
		require.NoError(t, os.Rename(filepath.Join(hidden, v.ID), filepath.Join(dir, v.ID)))
	}

	second, n := startDaemonStore(t, dir)
	assert.Equal(t, 2, n, "only the records the first run missed are migrated")
	assert.Len(t, second.List(), 3)
	require.NoError(t, second.Close())

	third, n := startDaemonStore(t, dir)
	assert.Zero(t, n, "a completed migration ran again")
	assert.Len(t, third.List(), 3, "a repeated migration duplicated or lost VMs")
}

func TestFaults_MigrationWhenTheDatabaseCannotBeWritten(t *testing.T) {
	dir, vms := fileRegistry(t, "app")
	dbPath := filepath.Join(dir, "vms.db")
	s := openSQLite(t, dbPath)
	unlock := lockDatabase(t, dbPath)

	n, err := NewMigrator(dir, s).Migrate()
	require.NoError(t, err)
	assert.Zero(t, n, "a record that was not written was counted as migrated")
	assert.Empty(t, s.List(), "a VM that could not be persisted must not be registered")
	// The source survives a failed migration.
	assert.FileExists(t, filepath.Join(dir, vms[0].ID, "state.json"))

	unlock()
	n, err = NewMigrator(dir, s).Migrate()
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.NoError(t, s.Close())
	assert.Equal(t, []string{vms[0].ID}, sqliteIDs(restoredSQLite(t, dbPath)))
}

func TestFaults_MigrationOfVMsThatWereRunning(t *testing.T) {
	for _, state := range []State{StateRunning, StateStarting} {
		t.Run(string(state), func(t *testing.T) {
			dir, vms := fileRegistry(t, "app")
			fs := NewFileStore(dir)
			require.NoError(t, fs.Restore())
			v, err := fs.Get(vms[0].ID)
			require.NoError(t, err)
			v.mu.Lock()
			v.State = state
			v.pid = 0x7ffffff0 // no such process
			v.mu.Unlock()
			require.NoError(t, fs.Save(v))

			s, n := startDaemonStore(t, dir)
			require.Equal(t, 1, n)
			got, err := s.Get(vms[0].ID)
			require.NoError(t, err)
			assert.Equal(t, StateStopped, got.GetState())
			assert.True(t, got.DaemonRecovered)
			select {
			case <-got.Done():
			default:
				t.Fatal("a VM migrated as stopped has an open done channel")
			}
			// The source record is left as it was, in case the upgrade is
			// rolled back.
			back := NewFileStore(dir)
			require.NoError(t, back.Restore())
			_, err = back.Get(vms[0].ID)
			require.NoError(t, err)
		})
	}
}

func TestFaults_MigrationSourceUnreadable(t *testing.T) {
	t.Run("root is a file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "vms")
		require.NoError(t, os.WriteFile(root, []byte("x"), 0o600))
		s := openSQLite(t, filepath.Join(t.TempDir(), "vms.db"))
		_, err := NewMigrator(root, s).Migrate()
		require.Error(t, err)
		assert.Empty(t, s.List())
	})
	t.Run("root is unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir, _ := fileRegistry(t, "app")
		s := openSQLite(t, filepath.Join(t.TempDir(), "vms.db"))
		require.NoError(t, os.Chmod(dir, 0o000))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		_, err := NewMigrator(dir, s).Migrate()
		require.Error(t, err, "an unreadable registry must not look like an empty one")
	})
}

func TestFaults_MigrationBringsBackRemovedVMs(t *testing.T) {
	dir, vms := fileRegistry(t, "app")
	s, _ := startDaemonStore(t, dir)
	require.NoError(t, s.Remove(vms[0].ID))
	require.NoError(t, s.Close())

	again, n := startDaemonStore(t, dir)
	assert.Zero(t, n)
	assert.Empty(t, again.List(), "a VM the user removed came back after a daemon restart")
}

// A database from before migrations were recorded holds migrated VMs with no
// record. The first start with this version must record them, so removing
// one afterwards is final.
func TestFaults_MigrationRecordsVMsMigratedByAnOlderVersion(t *testing.T) {
	dir, vms := fileRegistry(t, "app", "db")
	s, _ := startDaemonStore(t, dir)
	_, err := s.db.Exec("DELETE FROM migrated_vms")
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s, n := startDaemonStore(t, dir)
	require.Zero(t, n)
	require.NoError(t, s.Remove(vms[0].ID))
	require.NoError(t, s.Close())

	again, n := startDaemonStore(t, dir)
	assert.Zero(t, n)
	assert.Equal(t, []string{vms[1].ID}, sqliteIDs(again))
}

// Removal is final however many times the daemon starts afterwards.
func TestFaults_MigrationLeavesARemovedVMRemovedAcrossStarts(t *testing.T) {
	dir, vms := fileRegistry(t, "app")
	s, n := startDaemonStore(t, dir)
	require.Equal(t, 1, n)
	require.NoError(t, s.Remove(vms[0].ID))
	require.NoError(t, s.Close())
	for range 3 {
		s, n = startDaemonStore(t, dir)
		require.Zero(t, n)
		require.Empty(t, s.List())
		require.NoError(t, s.Close())
	}
}

// A VM created in the file registry after the first migration (the daemon
// was run with the file store again) is still migrated.
func TestFaults_MigrationTakesVMsAddedLater(t *testing.T) {
	dir, _ := fileRegistry(t, "app")
	s, n := startDaemonStore(t, dir)
	require.Equal(t, 1, n)
	require.NoError(t, s.Close())

	late, err := NewFileStore(dir).Create(Config{Name: "late", ImagePath: "a.img"})
	require.NoError(t, err)
	s, n = startDaemonStore(t, dir)
	require.Equal(t, 1, n)
	_, err = s.Get(late.ID)
	require.NoError(t, err)
}

// Both registries must carry the "recovered after a daemon restart" marker
// across further restarts: it is how the user learns why a VM is stopped.
func TestFaults_FileStoreKeepsRecoveredMarkerAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	s := restored(t, dir)
	v, err := s.Create(Config{Name: "app", ImagePath: "a.img"})
	require.NoError(t, err)
	v.mu.Lock()
	v.State = StateRunning
	v.pid = 0x7ffffff0
	v.mu.Unlock()
	require.NoError(t, s.Save(v))

	for round := range 3 {
		got, err := restored(t, dir).Get(v.ID)
		require.NoError(t, err)
		assert.Equal(t, StateStopped, got.GetState())
		assert.True(t, got.DaemonRecovered, "restart %d lost the marker", round+1)
	}
}
