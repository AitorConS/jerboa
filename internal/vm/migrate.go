//go:build linux || (darwin && arm64)

package vm

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

type Migrator struct {
	fileRoot string
	sqliteDB *SQLiteStore
}

func NewMigrator(fileRoot string, sqliteDB *SQLiteStore) *Migrator {
	return &Migrator{fileRoot: fileRoot, sqliteDB: sqliteDB}
}

func (m *Migrator) Migrate() (int, error) {
	entries, err := os.ReadDir(m.fileRoot)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("migration: no file store directory, nothing to migrate")
			return 0, nil
		}
		return 0, fmt.Errorf("migration: read dir: %w", err)
	}

	migrated := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		statePath := filepath.Join(m.fileRoot, e.Name(), "state.json")
		data, err := os.ReadFile(statePath)
		if err != nil {
			slog.Warn("migration: skip vm", "id", e.Name(), "err", err)
			continue
		}

		var st vmState
		if err := json.Unmarshal(data, &st); err != nil {
			slog.Warn("migration: parse vm state", "id", e.Name(), "err", err)
			continue
		}

		existing, err := m.sqliteDB.Get(st.ID)
		if err == nil && existing != nil {
			slog.Debug("migration: vm already exists in sqlite, skipping", "vm_id", st.ID)
			continue
		}
		// Migration runs before Restore, so memory alone does not say whether
		// the VM was migrated by an earlier start: the database does, and its
		// row is newer than the file record.
		inDB, err := m.sqliteDB.hasRow(st.ID)
		if err != nil {
			slog.Warn("migration: look up vm in sqlite", "vm_id", st.ID, "err", err)
			continue
		}
		if inDB {
			slog.Debug("migration: vm already exists in sqlite, skipping", "vm_id", st.ID)
			continue
		}

		v := &VM{
			ID:           st.ID,
			Cfg:          st.Config,
			State:        st.State,
			CreatedAt:    st.CreatedAt,
			StartedAt:    st.StartedAt,
			StoppedAt:    st.StoppedAt,
			RestartCount: st.RestartCount,
			done:         make(chan struct{}),
		}

		switch v.State {
		case StateRunning, StateStarting:
			slog.Info("migration: marking vm as stopped (daemon restart)", "vm_id", v.ID)
			v.State = StateStopped
			now := time.Now()
			v.StoppedAt = &now
			v.DaemonRecovered = true
			close(v.done)
		case StateStopped:
			close(v.done)
		default:
		}

		if st.DaemonRecovered {
			v.DaemonRecovered = true
		}

		// writeVM only persists registered VMs, so register first and take
		// the VM back out if its row cannot be written.
		m.sqliteDB.MemoryStore.mu.Lock()
		m.sqliteDB.vms[v.ID] = v
		m.sqliteDB.MemoryStore.mu.Unlock()
		if err := m.sqliteDB.writeVM(v); err != nil {
			slog.Warn("migration: write vm to sqlite", "vm_id", v.ID, "err", err)
			_ = m.sqliteDB.MemoryStore.Remove(v.ID)
			continue
		}

		slog.Info("migration: migrated vm", "vm_id", v.ID)
		migrated++
	}

	slog.Info("migration complete", "migrated", migrated)
	return migrated, nil
}
