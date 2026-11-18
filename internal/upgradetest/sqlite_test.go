//go:build linux || (darwin && arm64)

package upgradetest_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/upgradetest"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openSQLite opens the VM database of the second home the way the daemon
// does with --vm-store sqlite: the store, then the migration from the file
// registry beside it, then the restore.
func openSQLite(t *testing.T, f *upgradetest.Fixture) *vm.SQLiteStore {
	t.Helper()
	dir := filepath.Join(f.SQLiteHome(), ".jerboa", "vms")
	store, err := vm.NewSQLiteStore(filepath.Join(dir, "vms.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	_, err = vm.NewMigrator(dir, store).Migrate()
	require.NoError(t, err)
	require.NoError(t, store.Restore())
	return store
}

func vmNames(store *vm.SQLiteStore) []string {
	var names []string
	for _, v := range store.List() {
		names = append(names, v.Cfg.Name)
	}
	return names
}

// TestSQLiteStoreOfAReleasedVersion opens a vms.db the released daemon
// created and used: world-readable, and without the table that records which
// VMs were taken over from the file registry.
func TestSQLiteStoreOfAReleasedVersion(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		dir := filepath.Join(f.SQLiteHome(), ".jerboa", "vms")
		db := filepath.Join(dir, "vms.db")
		before, err := os.Stat(db)
		require.NoError(t, err)
		require.NotZero(t, before.Mode().Perm()&0o077, "the released version left the database readable by others")

		store := openSQLite(t, f)
		// The database holds every VM's environment: owner-only from the
		// first start, without waiting for a write.
		for _, path := range []string{db, dir} {
			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Zero(t, info.Mode().Perm()&0o077, "%s after the upgrade", path)
		}

		want := f.Expected.SQLite
		byName := map[string]*vm.VM{}
		for _, v := range store.List() {
			byName[v.Cfg.Name] = v
		}
		for name, recorded := range want.VMs {
			v, ok := byName[name]
			require.True(t, ok, "VM %q is in the released database", name)
			assertSameVM(t, recorded, v)
			assert.Equal(t, vm.StateStopped, v.State, name)
		}
		// The released version migrated the file registry into memory only:
		// its database holds the VMs created after the move and none of the
		// migrated ones, which it migrated again at every start. They get
		// their rows now.
		assert.Equal(t, len(want.VMs)+1, countRows(t, db, "vms"))
		// For the same reason a VM removed under the released version left
		// its state.json behind and came back at every start. Nothing on
		// disk tells it apart from a VM that was never migrated, so it comes
		// back this once more...
		require.NotEmpty(t, want.RemovedButLeftOnDisk)
		ghost, err := store.Get(want.RemovedButLeftOnDisk)
		require.NoError(t, err, "a VM the file registry still holds is migrated")
		assert.Len(t, store.List(), len(want.VMs)+1)
		// ...and removing it now is final.
		require.NoError(t, store.Remove(ghost.ID))
		require.NoError(t, store.Close())
		again := openSQLite(t, f)
		assert.ElementsMatch(t, keys(want.VMs), vmNames(again))
	})
}

// TestSQLiteMigrationInterrupted cuts the first start after the upgrade at
// the points where the database and the migration record disagree, and
// starts again: every VM is there once, and a removed one stays removed.
func TestSQLiteMigrationInterrupted(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		dir := filepath.Join(f.SQLiteHome(), ".jerboa", "vms")
		want := f.Expected.SQLite

		db := filepath.Join(dir, "vms.db")
		// The daemon died after creating the new table and before migrating
		// anything: the schema is new, the record is empty.
		store, err := vm.NewSQLiteStore(db)
		require.NoError(t, err)
		require.NoError(t, store.Close())
		require.Equal(t, 0, countRows(t, db, "migrated_vms"))
		first := openSQLite(t, f)
		assert.ElementsMatch(t, append(keys(want.VMs), "removed"), vmNames(first))
		require.NoError(t, first.Close())
		require.Equal(t, 2, countRows(t, db, "migrated_vms"))

		// It died between writing a VM's row and recording the migration:
		// rows without a record. The next start must neither duplicate nor
		// lose them, and must complete the record.
		raw, err := sql.Open("sqlite", filepath.Join(dir, "vms.db"))
		require.NoError(t, err)
		_, err = raw.Exec("DELETE FROM migrated_vms") //nolint:noctx // test setup
		require.NoError(t, err)
		require.NoError(t, raw.Close())
		second := openSQLite(t, f)
		assert.ElementsMatch(t, append(keys(want.VMs), "removed"), vmNames(second))
		assert.Equal(t, len(want.VMs)+1, countRows(t, db, "vms"), "no VM is duplicated or lost")
		assert.Equal(t, 2, countRows(t, db, "migrated_vms"), "the record is completed")
		require.NoError(t, second.Remove(want.RemovedButLeftOnDisk))
		require.NoError(t, second.Close())

		third := openSQLite(t, f)
		assert.ElementsMatch(t, keys(want.VMs), vmNames(third), "a removed VM does not come back from the file registry")
	})
}

// TestSQLiteDatabaseDamaged gives the daemon a database that is not one: the
// store must refuse it by name and leave the file as it found it, so the
// operator can still recover it.
func TestSQLiteDatabaseDamaged(t *testing.T) {
	upgradetest.Each(t, func(t *testing.T, f *upgradetest.Fixture) {
		db := filepath.Join(f.SQLiteHome(), ".jerboa", "vms", "vms.db")
		data, err := os.ReadFile(db)
		require.NoError(t, err)
		damaged := append([]byte("this is not a database"), data[22:len(data)/2]...)
		require.NoError(t, os.WriteFile(db, damaged, 0o600))
		_, err = vm.NewSQLiteStore(db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sqlite")
		after, err := os.ReadFile(db)
		require.NoError(t, err)
		assert.Equal(t, damaged, after, "a database that cannot be opened is not rewritten")
	})
}

func countRows(t *testing.T, db, table string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	var n int
	require.NoError(t, raw.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n)) //nolint:noctx,gosec // test query on a fixed table name
	return n
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
