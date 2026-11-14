//go:build linux || (darwin && arm64)

package vm

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	MemoryStore
	db *sql.DB
	mu sync.Mutex
}

func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	dir := filepath.Dir(dsn)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("sqlite mkdir: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &SQLiteStore{
		MemoryStore: *NewMemoryStore(),
		db:          db,
	}
	if err := s.createSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite schema: %w", err)
	}
	return s, nil
}

func (s *SQLiteStore) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close sqlite: %w", err)
	}
	return nil
}

const sqlCreateSchema = `
	CREATE TABLE IF NOT EXISTS vms (
		id              TEXT PRIMARY KEY,
		config          TEXT NOT NULL,
		state           TEXT NOT NULL,
		created_at      TEXT NOT NULL,
		started_at      TEXT,
		stopped_at      TEXT,
		daemon_recovered INTEGER DEFAULT 0,
		health_status   TEXT DEFAULT '',
		restart_count   INTEGER DEFAULT 0,
		pid             INTEGER DEFAULT 0
	)
`

// sqlCreateMigrated records the VMs already taken over from the file
// registry. The record outlives the VM's own row, so a VM removed later is
// not migrated a second time from the state.json it left behind.
const sqlCreateMigrated = `
	CREATE TABLE IF NOT EXISTS migrated_vms (
		id TEXT PRIMARY KEY
	)
`

const sqlUpsertVM = `
	INSERT INTO vms (id, config, state, created_at, started_at, stopped_at, daemon_recovered, health_status, restart_count, pid)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		config = excluded.config,
		state = excluded.state,
		started_at = excluded.started_at,
		stopped_at = excluded.stopped_at,
		daemon_recovered = excluded.daemon_recovered,
		health_status = excluded.health_status,
		restart_count = excluded.restart_count,
		pid = excluded.pid
`

func (s *SQLiteStore) createSchema() error {
	if _, err := s.db.Exec(sqlCreateSchema); err != nil { //nolint:noctx // schema creation at startup; no per-request context
		return fmt.Errorf("create table: %w", err)
	}
	// Migrate databases created before the pid column existed. SQLite has no
	// "ADD COLUMN IF NOT EXISTS", so ignore the duplicate-column error.
	if _, err := s.db.Exec("ALTER TABLE vms ADD COLUMN pid INTEGER DEFAULT 0"); err != nil && //nolint:noctx // startup migration; no per-request context
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add pid column: %w", err)
	}
	if _, err := s.db.Exec(sqlCreateMigrated); err != nil { //nolint:noctx // schema creation at startup; no per-request context
		return fmt.Errorf("create migration table: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Create(cfg Config) (*VM, error) {
	v, err := s.MemoryStore.Create(cfg)
	if err != nil {
		return nil, err
	}
	if err := s.writeVM(v); err != nil {
		_ = s.MemoryStore.Remove(v.ID)
		return nil, fmt.Errorf("persist vm %s: %w", v.ID, err)
	}
	return v, nil
}

func (s *SQLiteStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Get(id); err != nil {
		return fmt.Errorf("vm %q not found", id)
	}
	// Delete the row first: a VM whose row could not be deleted stays
	// registered, instead of vanishing now and coming back at the next start.
	_, err := s.db.Exec("DELETE FROM vms WHERE id = ?", id) //nolint:noctx // store Remove has no context parameter
	if err != nil {
		return fmt.Errorf("delete vm %s: %w", id, err)
	}
	return s.MemoryStore.Remove(id)
}

func (s *SQLiteStore) Save(v *VM) error {
	return s.writeVM(v)
}

func (s *SQLiteStore) Restore() error {
	s.mu.Lock()

	rows, err := s.db.Query("SELECT id, config, state, created_at, started_at, stopped_at, daemon_recovered, health_status, restart_count, pid FROM vms") //nolint:noctx // RestoreAll is called at daemon startup with no context
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("restore: query: %w", err)
	}
	defer rows.Close()

	// VMs whose in-memory state diverged from the DB during recovery; persisted
	// after the lock is released since Save re-acquires s.mu.
	var toSave []*VM

	for rows.Next() {
		var id, configJSON, stateStr, createdAtStr string
		var startedAtStr, stoppedAtStr sql.NullString
		var daemonRecovered int
		var healthStatus string
		var restartCount int
		var pid int

		if err := rows.Scan(&id, &configJSON, &stateStr, &createdAtStr, &startedAtStr, &stoppedAtStr, &daemonRecovered, &healthStatus, &restartCount, &pid); err != nil {
			slog.Warn("restore: scan row", "err", err)
			continue
		}

		var cfg Config
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			slog.Warn("restore: parse config", "id", id, "err", err)
			continue
		}

		createdAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
		if err != nil {
			slog.Warn("restore: parse created_at", "id", id, "err", err)
			continue
		}

		v := &VM{
			ID:              id,
			Cfg:             cfg,
			State:           State(stateStr),
			CreatedAt:       createdAt,
			RestartCount:    restartCount,
			DaemonRecovered: daemonRecovered != 0,
			done:            make(chan struct{}),
		}

		if startedAtStr.Valid {
			t, err := time.Parse(time.RFC3339Nano, startedAtStr.String)
			if err == nil {
				v.StartedAt = &t
			}
		}
		if stoppedAtStr.Valid {
			t, err := time.Parse(time.RFC3339Nano, stoppedAtStr.String)
			if err == nil {
				v.StoppedAt = &t
			}
		}

		if healthStatus != "" {
			v.HealthStatus = HealthStatus(healthStatus)
		}

		switch v.State {
		case StateRunning, StateStarting, StateStopping:
			// A stop in progress when the daemon exited is resolved like a
			// running VM: re-adopted if its process survived, else stopped.
			// Re-adopt the process if it survived the daemon; otherwise mark
			// the VM stopped and flag it as daemon-recovered.
			if recoverVM(s, v, pid) {
				toSave = append(toSave, v)
			}
		case StateStopped:
			close(v.done)
		default:
		}

		s.vms[v.ID] = v
	}
	rowsErr := rows.Err()
	s.mu.Unlock()

	if rowsErr != nil {
		return fmt.Errorf("restore: rows: %w", rowsErr)
	}

	// Persist the recovered stopped state so a dead VM is not re-recovered (and
	// its network re-reconciled) on every subsequent daemon restart.
	for _, v := range toSave {
		if err := s.Save(v); err != nil {
			slog.Warn("restore: persist recovered vm", "vm_id", v.ID, "err", err)
		}
	}
	return nil
}

func (s *SQLiteStore) writeVM(v *VM) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.Get(v.ID); err != nil {
		return nil
	}
	v.mu.RLock()
	cfg, err := json.Marshal(v.Cfg)
	if err != nil {
		v.mu.RUnlock()
		return fmt.Errorf("marshal config: %w", err)
	}
	st := vmState{
		ID:           v.ID,
		Config:       v.Cfg,
		State:        v.State,
		CreatedAt:    v.CreatedAt,
		StartedAt:    v.StartedAt,
		StoppedAt:    v.StoppedAt,
		RestartCount: v.RestartCount,
		PID:          v.pid,
	}
	if v.DaemonRecovered {
		st.DaemonRecovered = true
	}
	health := string(v.HealthStatus)
	v.mu.RUnlock()

	_, dbErr := s.db.Exec(sqlUpsertVM, //nolint:noctx // writeVM is called from the VM state machine with no context
		st.ID, string(cfg), string(st.State), st.CreatedAt.Format(time.RFC3339Nano),
		nullTime(st.StartedAt), nullTime(st.StoppedAt), boolToInt(st.DaemonRecovered), health, st.RestartCount, st.PID)

	if dbErr != nil {
		return fmt.Errorf("upsert vm %s: %w", v.ID, dbErr)
	}
	return nil
}

// hasRow reports whether the database holds a row for id, whether or not the
// VM is loaded in memory.
func (s *SQLiteStore) hasRow(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM vms WHERE id = ?", id).Scan(&n); err != nil { //nolint:noctx // startup migration; no per-request context
		return false, fmt.Errorf("look up vm %s: %w", id, err)
	}
	return n > 0, nil
}

// wasMigrated reports whether the VM was already taken over from the file
// registry by this or an earlier start.
func (s *SQLiteStore) wasMigrated(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM migrated_vms WHERE id = ?", id).Scan(&n); err != nil { //nolint:noctx // startup migration; no per-request context
		return false, fmt.Errorf("look up migration of vm %s: %w", id, err)
	}
	return n > 0, nil
}

// markMigrated records that the VM needs no further migration.
func (s *SQLiteStore) markMigrated(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec("INSERT OR IGNORE INTO migrated_vms (id) VALUES (?)", id); err != nil { //nolint:noctx // startup migration; no per-request context
		return fmt.Errorf("record migration of vm %s: %w", id, err)
	}
	return nil
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339Nano)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
