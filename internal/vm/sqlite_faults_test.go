//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The SQLite VM registry after things go wrong: a database file that is not a
// database, one another process holds locked, a row a bad write damaged, a
// daemon killed mid-transaction. One damaged row must cost at most that VM, a
// failed write must not leave memory and disk disagreeing, and the daemon must
// always be able to tell "empty" from "unreadable".

func openSQLite(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// restoredSQLite opens the database at path the way a starting daemon does.
func restoredSQLite(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	s := openSQLite(t, path)
	require.NoError(t, s.Restore())
	return s
}

func sqliteIDs(s *SQLiteStore) []string {
	var out []string
	for _, v := range s.List() {
		out = append(out, v.ID)
	}
	sort.Strings(out)
	return out
}

// lockDatabase takes the write lock of the database at path from a second
// connection, as another process (a second daemon, a backup tool) would, and
// returns the function that releases it.
func lockDatabase(t *testing.T, path string) (unlock func()) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	var once sync.Once
	unlock = func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			_ = db.Close()
		})
	}
	t.Cleanup(unlock)
	return unlock
}

func TestFaults_SQLiteMissingDatabaseIsCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "vms.db")
	s := restoredSQLite(t, path)
	assert.Empty(t, s.List())
	v, err := s.Create(Config{Name: "first", ImagePath: "a.img"})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	assert.Equal(t, []string{v.ID}, sqliteIDs(restoredSQLite(t, path)))
}

func TestFaults_SQLiteRefusesAFileThatIsNotADatabase(t *testing.T) {
	contents := map[string][]byte{
		"garbage":          []byte("this is not a database, it is a text file that ended up here"),
		"zeroes":           make([]byte, 8192),
		"truncated header": []byte("SQLite format 3\x00"),
	}
	for name, data := range contents {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vms.db")
			require.NoError(t, os.WriteFile(path, data, 0o600))

			s, err := NewSQLiteStore(path)
			if err == nil {
				// Opening is lazy for some damage; the first read must fail.
				err = s.Restore()
				_ = s.Close()
			}
			require.Error(t, err, "a file that is not a database was accepted as an empty registry")

			// The damaged file is evidence, and maybe recoverable: it must
			// not be replaced or emptied by the failed open.
			got, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, data, got, "the unreadable database was modified")
		})
	}
}

func TestFaults_SQLiteDatabaseCutInHalf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vms.db")
	s := restoredSQLite(t, path)
	// Enough rows that the table spans many pages.
	for i := range 200 {
		_, err := s.Create(Config{Name: fmt.Sprintf("vm-%d", i), ImagePath: "a.img", Env: []string{"PAD=" + fmt.Sprintf("%0400d", i)}})
		require.NoError(t, err)
	}
	require.NoError(t, s.Close())
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(16384))
	// A power cut on a filesystem that lost the tail of the file.
	require.NoError(t, os.Truncate(path, info.Size()/2))

	again, err := NewSQLiteStore(path)
	if err == nil {
		err = again.Restore()
		if err == nil {
			// Whatever was read back must be a strict, self-consistent part
			// of what was written: no invented or duplicated VMs.
			assert.Less(t, len(again.List()), 200)
			for _, v := range again.List() {
				assert.NotEmpty(t, v.ID)
				assert.Equal(t, "a.img", v.Cfg.ImagePath)
			}
		}
		_ = again.Close()
	}
	// Either outcome is acceptable as long as it did not report a full,
	// healthy registry; an error is the expected one.
	if err == nil {
		t.Log("truncated database opened with a partial registry")
	}
}

func TestFaults_SQLitePathThatCannotHoldADatabase(t *testing.T) {
	t.Run("path is a directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "vms.db")
		require.NoError(t, os.Mkdir(path, 0o700))
		_, err := NewSQLiteStore(path)
		require.Error(t, err)
	})
	t.Run("parent is a file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "state")
		require.NoError(t, os.WriteFile(parent, []byte("x"), 0o600))
		_, err := NewSQLiteStore(filepath.Join(parent, "vms.db"))
		require.Error(t, err)
	})
	t.Run("directory is read-only", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		_, err := NewSQLiteStore(filepath.Join(dir, "vms.db"))
		require.Error(t, err)
	})
}

func TestFaults_SQLiteReadOnlyDatabaseFailsWritesAndKeepsData(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	path := filepath.Join(t.TempDir(), "vms.db")
	s := restoredSQLite(t, path)
	keep, err := s.Create(Config{Name: "keep", ImagePath: "a.img"})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	// The filesystem was remounted read-only after an I/O error.
	require.NoError(t, os.Chmod(path, 0o400))
	require.NoError(t, os.Chmod(filepath.Dir(path), 0o500))
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Dir(path), 0o700)
		_ = os.Chmod(path, 0o600)
	})

	ro, err := NewSQLiteStore(path)
	if err != nil {
		return // refusing to start on a read-only registry is fine
	}
	t.Cleanup(func() { _ = ro.Close() })
	require.NoError(t, ro.Restore())
	assert.Equal(t, []string{keep.ID}, sqliteIDs(ro), "the registry must still be readable")
	_, err = ro.Create(Config{Name: "fresh", ImagePath: "b.img"})
	require.Error(t, err)
	assert.Equal(t, []string{keep.ID}, sqliteIDs(ro), "a VM that could not be persisted must not stay registered")
}

func TestFaults_SQLiteLockedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vms.db")
	s := restoredSQLite(t, path)
	keep, err := s.Create(Config{Name: "keep", ImagePath: "a.img"})
	require.NoError(t, err)
	keep.mu.Lock()
	keep.RestartCount = 1
	keep.mu.Unlock()
	require.NoError(t, s.Save(keep))

	unlock := lockDatabase(t, path)

	t.Run("create is all or nothing", func(t *testing.T) {
		_, err := s.Create(Config{Name: "fresh", ImagePath: "b.img"})
		require.Error(t, err)
		assert.Equal(t, []string{keep.ID}, sqliteIDs(s), "a VM that could not be persisted must not stay registered")
	})
	t.Run("save reports the failure", func(t *testing.T) {
		keep.mu.Lock()
		keep.RestartCount = 2
		keep.mu.Unlock()
		require.Error(t, s.Save(keep), "a state change that was not written must not be reported as saved")
	})
	t.Run("remove keeps the VM it could not delete", func(t *testing.T) {
		require.Error(t, s.Remove(keep.ID))
		_, err := s.Get(keep.ID)
		require.NoError(t, err, "the VM vanished from the registry although its record is still on disk")
	})
	t.Run("restore reports the failure", func(t *testing.T) {
		other, err := NewSQLiteStore(path)
		if err == nil {
			err = other.Restore()
			_ = other.Close()
		}
		require.Error(t, err, "a registry that could not be read must not look empty")
	})

	unlock()

	// Nothing was half-applied while the database was locked.
	again := restoredSQLite(t, path)
	assert.Equal(t, []string{keep.ID}, sqliteIDs(again))
	got, err := again.Get(keep.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.RestartCount, "the record on disk must be the last one that was saved")
	require.NoError(t, again.Close())

	// And the original store works again without being reopened.
	require.NoError(t, s.Save(keep))
	_, err = s.Create(Config{Name: "fresh", ImagePath: "b.img"})
	require.NoError(t, err)
	require.NoError(t, s.Remove(keep.ID))
	final := restoredSQLite(t, path)
	require.Len(t, final.List(), 1)
	assert.Equal(t, "fresh", final.List()[0].Cfg.Name)
}

func TestFaults_SQLiteClosedStoreFailsInsteadOfPanicking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vms.db")
	s := restoredSQLite(t, path)
	v, err := s.Create(Config{Name: "app", ImagePath: "a.img"})
	require.NoError(t, err)
	require.NoError(t, s.Close())

	// The daemon is shutting down while a monitor goroutine still reports a
	// state change.
	require.Error(t, s.Save(v))
	_, err = s.Create(Config{Name: "late", ImagePath: "b.img"})
	require.Error(t, err)
	require.Error(t, s.Restore())
	require.Error(t, s.Remove(v.ID))

	assert.Equal(t, []string{v.ID}, sqliteIDs(restoredSQLite(t, path)))
}

func TestFaults_SQLiteDamagedRowCostsOnlyThatVM(t *testing.T) {
	damage := map[string]string{
		"config is not JSON":      `UPDATE vms SET config = '{"Name":"bro' WHERE id = ?`,
		"config is empty":         `UPDATE vms SET config = '' WHERE id = ?`,
		"config has wrong types":  `UPDATE vms SET config = '{"Name":7,"CPUs":"many"}' WHERE id = ?`,
		"created_at is garbage":   `UPDATE vms SET created_at = 'yesterday' WHERE id = ?`,
		"created_at is empty":     `UPDATE vms SET created_at = '' WHERE id = ?`,
		"health_status is NULL":   `UPDATE vms SET health_status = NULL WHERE id = ?`,
		"restart_count is NULL":   `UPDATE vms SET restart_count = NULL WHERE id = ?`,
		"restart_count is text":   `UPDATE vms SET restart_count = 'lots' WHERE id = ?`,
		"pid is text":             `UPDATE vms SET pid = 'none' WHERE id = ?`,
		"daemon_recovered is NUL": `UPDATE vms SET daemon_recovered = NULL WHERE id = ?`,
	}
	for name, stmt := range damage {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vms.db")
			s := restoredSQLite(t, path)
			broken, err := s.Create(Config{Name: "broken", ImagePath: "a.img"})
			require.NoError(t, err)
			fine, err := s.Create(Config{Name: "fine", ImagePath: "b.img"})
			require.NoError(t, err)
			res, err := s.db.Exec(stmt, broken.ID)
			require.NoError(t, err)
			n, err := res.RowsAffected()
			require.NoError(t, err)
			require.Equal(t, int64(1), n)
			require.NoError(t, s.Close())

			again := openSQLite(t, path)
			require.NoError(t, again.Restore(), "one damaged row must not stop the daemon from starting")
			assert.Equal(t, []string{fine.ID}, sqliteIDs(again))
			got, err := again.Get(fine.ID)
			require.NoError(t, err)
			assert.Equal(t, "fine", got.Cfg.Name)

			// The registry keeps working: new VMs, and removal of the survivor.
			_, err = again.Create(Config{Name: "new", ImagePath: "c.img"})
			require.NoError(t, err)
			require.NoError(t, again.Remove(fine.ID))
		})
	}
}

func TestFaults_SQLiteDamagedOptionalFieldsDoNotCostTheVM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vms.db")
	s := restoredSQLite(t, path)
	v, err := s.Create(Config{Name: "app", ImagePath: "a.img"})
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE vms SET state = 'stopped', started_at = 'not a time', stopped_at = '' WHERE id = ?`, v.ID)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	again := restoredSQLite(t, path)
	got, err := again.Get(v.ID)
	require.NoError(t, err, "unreadable timestamps must not lose the VM")
	assert.Equal(t, StateStopped, got.GetState())
	started, stopped := got.GetTimes()
	assert.Nil(t, started)
	assert.Nil(t, stopped)
	select {
	case <-got.Done():
	default:
		t.Fatal("a stopped VM restored with an open done channel blocks everyone who waits for it")
	}
}

func TestFaults_SQLiteDatabaseFromBeforeThePIDColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vms.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE vms (
		id TEXT PRIMARY KEY, config TEXT NOT NULL, state TEXT NOT NULL, created_at TEXT NOT NULL,
		started_at TEXT, stopped_at TEXT, daemon_recovered INTEGER DEFAULT 0,
		health_status TEXT DEFAULT '', restart_count INTEGER DEFAULT 0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO vms (id, config, state, created_at) VALUES ('aaaaaaaaaaaa', '{"Name":"old","ImagePath":"a.img"}', 'running', ?)`,
		time.Now().Format(time.RFC3339Nano))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// The upgrade runs every time the store is opened; an interrupted or
	// repeated one must be harmless.
	for round := range 3 {
		s := openSQLite(t, path)
		require.NoError(t, s.Restore(), "round %d", round)
		got, err := s.Get("aaaaaaaaaaaa")
		require.NoError(t, err)
		assert.Equal(t, "old", got.Cfg.Name)
		// It was running under a daemon that recorded no pid: nothing to
		// adopt, so it is stopped.
		assert.Equal(t, StateStopped, got.GetState())
		assert.True(t, got.DaemonRecovered)
		require.NoError(t, s.Close())
	}
}

func TestFaults_SQLiteDaemonRestartInEveryState(t *testing.T) {
	cases := []struct {
		state     State
		want      State
		recovered bool
		doneOpen  bool
	}{
		{StateCreated, StateCreated, false, true},
		{StateStarting, StateStopped, true, false},
		{StateRunning, StateStopped, true, false},
		{StateStopping, StateStopped, true, false},
		{StateStopped, StateStopped, false, false},
		// An interrupted in-place restore is resolved by the snapshot
		// journal, not by the registry: it must come back untouched.
		{StateRestoring, StateRestoring, false, true},
		{State("from-a-newer-daemon"), State("from-a-newer-daemon"), false, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vms.db")
			s := restoredSQLite(t, path)
			v, err := s.Create(Config{Name: "app", ImagePath: "a.img"})
			require.NoError(t, err)
			v.mu.Lock()
			v.State = tc.state
			v.pid = 0x7ffffff0 // no such process: the machine was power-cycled
			v.HealthStatus = HealthHealthy
			v.RestartCount = 4
			v.mu.Unlock()
			require.NoError(t, s.Save(v))
			require.NoError(t, s.Close())

			again := restoredSQLite(t, path)
			got, err := again.Get(v.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.GetState())
			assert.Equal(t, tc.recovered, got.DaemonRecovered)
			assert.Equal(t, 4, got.GetRestartCount())
			select {
			case <-got.Done():
				assert.False(t, tc.doneOpen, "done closed for a VM that is not stopped")
			default:
				assert.True(t, tc.doneOpen, "done still open for a stopped VM")
			}
			if tc.recovered {
				_, stoppedAt := got.GetTimes()
				assert.NotNil(t, stoppedAt)
			}
			require.NoError(t, again.Close())

			// The recovered state was written back, so the next start does
			// not recover it a second time.
			third := restoredSQLite(t, path)
			got, err = third.Get(v.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.GetState())
			assert.Equal(t, tc.recovered, got.DaemonRecovered)
		})
	}
}

func TestFaults_SQLiteRecordRoundTripsEveryField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vms.db")
	s := restoredSQLite(t, path)
	cfg := Config{
		Name: "app", ImagePath: "a.img", ImageRef: "app:v1", Memory: "512M", CPUs: 2,
		Env: []string{"TOKEN=secret"}, NetworkName: "net0", IPAddress: "10.1.0.5",
		PortMaps:    []PortMap{{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}},
		Volumes:     []VolumeMount{{DiskPath: "/v/d.img", GuestPath: "/data", Label: "data"}},
		HealthCheck: &HealthCheckConfig{Type: "tcp", Port: 80, Interval: time.Second, Retries: 5},
		Restart:     RestartConfig{Policy: RestartOnFailure, MaxRetries: 3},
	}
	v, err := s.Create(cfg)
	require.NoError(t, err)
	started := time.Now().Add(-time.Hour).UTC()
	stopped := time.Now().UTC()
	v.mu.Lock()
	v.State = StateStopped
	v.StartedAt = &started
	v.StoppedAt = &stopped
	v.HealthStatus = HealthUnhealthy
	v.RestartCount = 2
	v.DaemonRecovered = true
	v.mu.Unlock()
	require.NoError(t, s.Save(v))
	require.NoError(t, s.Close())

	got, err := restoredSQLite(t, path).Get(v.ID)
	require.NoError(t, err)
	assert.Equal(t, cfg, got.Cfg)
	assert.Equal(t, StateStopped, got.GetState())
	gotStarted, gotStopped := got.GetTimes()
	require.NotNil(t, gotStarted)
	require.NotNil(t, gotStopped)
	assert.True(t, started.Equal(*gotStarted))
	assert.True(t, stopped.Equal(*gotStopped))
	assert.Equal(t, HealthUnhealthy, got.GetHealthStatus())
	assert.Equal(t, 2, got.GetRestartCount())
	assert.WithinDuration(t, v.CreatedAt, got.CreatedAt, time.Millisecond)
}

func TestFaults_SQLiteSaveOfARemovedVMDoesNotBringItBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vms.db")
	s := restoredSQLite(t, path)
	a, err := s.Create(Config{Name: "a", ImagePath: "a.img"})
	require.NoError(t, err)
	b, err := s.Create(Config{Name: "b", ImagePath: "b.img"})
	require.NoError(t, err)
	require.NoError(t, s.Remove(a.ID))
	// The monitor goroutine of a VM reports its last state change after the
	// user removed it.
	require.NoError(t, s.Save(a))
	require.Error(t, s.Remove(a.ID), "removing twice must fail, not succeed silently")
	require.NoError(t, s.Close())

	assert.Equal(t, []string{b.ID}, sqliteIDs(restoredSQLite(t, path)))
}

func TestFaults_SQLiteConcurrentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vms.db")
	s := restoredSQLite(t, path)
	const workers, rounds = 8, 15
	kept := make([][]string, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				v, err := s.Create(Config{Name: fmt.Sprintf("w%d-%d", w, i), ImagePath: "a.img"})
				if !assert.NoError(t, err) {
					return
				}
				v.mu.Lock()
				v.RestartCount = i
				v.mu.Unlock()
				assert.NoError(t, s.Save(v))
				_ = s.List()
				if i%3 == 0 {
					assert.NoError(t, s.Remove(v.ID))
					// A late save from the VM's monitor.
					assert.NoError(t, s.Save(v))
					continue
				}
				kept[w] = append(kept[w], v.ID)
			}
		}()
	}
	wg.Wait()
	var want []string
	for _, ids := range kept {
		want = append(want, ids...)
	}
	sort.Strings(want)
	assert.Equal(t, want, sqliteIDs(s))
	require.NoError(t, s.Close())
	assert.Equal(t, want, sqliteIDs(restoredSQLite(t, path)), "memory and disk disagree after concurrent use")
}

// A daemon killed at arbitrary points of its writes must leave a database
// that opens, with every committed VM intact.
func TestFaults_SQLiteSurvivesKill(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns and kills helper processes")
	}
	path := filepath.Join(t.TempDir(), "vms.db")
	s := restoredSQLite(t, path)
	base, err := s.Create(Config{Name: "base", ImagePath: "base.img", Env: []string{"KEEP=1"}})
	require.NoError(t, err)
	require.NoError(t, s.Close())

	for round := range 10 {
		ready := filepath.Join(t.TempDir(), "ready")
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperChurnSQLite$")
		cmd.Env = append(os.Environ(), "VM_SQLITE_HELPER_DB="+path, "VM_SQLITE_HELPER_READY="+ready)
		require.NoError(t, cmd.Start())
		require.Eventually(t, func() bool {
			_, err := os.Stat(ready)
			return err == nil
		}, 30*time.Second, 2*time.Millisecond, "helper never started")
		time.Sleep(time.Duration(2+round*4) * time.Millisecond)
		require.NoError(t, cmd.Process.Kill())
		_ = cmd.Wait()

		again, err := NewSQLiteStore(path)
		require.NoError(t, err, "round %d: the database does not open after a kill", round)
		require.NoError(t, again.Restore(), "round %d", round)
		got, err := again.Get(base.ID)
		require.NoError(t, err, "round %d: the VM that was never touched is gone", round)
		assert.Equal(t, []string{"KEEP=1"}, got.Cfg.Env)
		for _, v := range again.List() {
			assert.NotEmpty(t, v.Cfg.ImagePath, "round %d: VM %s restored with an empty config", round, v.ID)
		}
		var check string
		require.NoError(t, again.db.QueryRow("PRAGMA integrity_check").Scan(&check))
		assert.Equal(t, "ok", check, "round %d", round)
		require.NoError(t, again.Close())
	}
}

func TestHelperChurnSQLite(t *testing.T) {
	path := os.Getenv("VM_SQLITE_HELPER_DB")
	if path == "" {
		t.Skip("helper process for TestFaults_SQLiteSurvivesKill")
	}
	s, err := NewSQLiteStore(path)
	if err != nil {
		os.Exit(3)
	}
	if err := s.Restore(); err != nil {
		os.Exit(3)
	}
	_ = os.WriteFile(os.Getenv("VM_SQLITE_HELPER_READY"), nil, 0o600)
	for i := 0; ; i++ {
		v, err := s.Create(Config{Name: fmt.Sprintf("churn%d", i), ImagePath: "churn.img"})
		if err != nil {
			continue
		}
		v.mu.Lock()
		v.RestartCount = i
		v.mu.Unlock()
		_ = s.Save(v)
		if i%2 == 0 {
			_ = s.Remove(v.ID)
		}
	}
}
