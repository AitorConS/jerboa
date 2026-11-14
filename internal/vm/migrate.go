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

		// A VM is migrated once. Without this record a VM removed after its
		// migration would be migrated again from the state.json it left in
		// the file registry, and come back at every start.
		done, err := m.sqliteDB.wasMigrated(st.ID)
		if err != nil {
			slog.Warn("migration: look up vm migration", "vm_id", st.ID, "err", err)
			continue
		}
		if done {
			continue
		}
		existing, err := m.sqliteDB.Get(st.ID)
		if err == nil && existing != nil {
			slog.Debug("migration: vm already exists in sqlite, skipping", "vm_id", st.ID)
			m.recordMigrated(st.ID)
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
			// Migrated by a start that predates the record, or one that was
			// cut between writing the row and recording it.
			slog.Debug("migration: vm already exists in sqlite, skipping", "vm_id", st.ID)
			m.recordMigrated(st.ID)
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

		m.recordMigrated(v.ID)
		slog.Info("migration: migrated vm", "vm_id", v.ID)
		migrated++
	}

	slog.Info("migration complete", "migrated", migrated)
	return migrated, nil
}

// recordMigrated marks a VM as migrated. A failure is only logged: the VM's
// row keeps it from being migrated again for as long as the VM exists, and
// the next start retries the record.
func (m *Migrator) recordMigrated(id string) {
	if err := m.sqliteDB.markMigrated(id); err != nil {
		slog.Warn("migration: record vm as migrated", "vm_id", id, "err", err)
	}
}
