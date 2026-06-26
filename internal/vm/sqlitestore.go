//go:build linux

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
	if err := s.MemoryStore.Remove(id); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM vms WHERE id = ?", id) //nolint:noctx // store Remove has no context parameter
	if err != nil {
		return fmt.Errorf("delete vm %s: %w", id, err)
	}
	return nil
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
			ID:           id,
			Cfg:          cfg,
			State:        State(stateStr),
			CreatedAt:    createdAt,
			RestartCount: restartCount,
			done:         make(chan struct{}),
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
		case StateRunning, StateStarting:
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
