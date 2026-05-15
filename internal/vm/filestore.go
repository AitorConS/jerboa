package vm

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// vmState is the on-disk representation of a VM, used for persistence.
type vmState struct {
	ID              string     `json:"id"`
	Config          Config     `json:"config"`
	State           State      `json:"state"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	StoppedAt       *time.Time `json:"stopped_at,omitempty"`
	DaemonRecovered bool       `json:"daemon_recovered,omitempty"`
	HealthStatus    string     `json:"health_status,omitempty"`
	RestartCount    int        `json:"restart_count,omitempty"`
}

// FileStore is a Store that persists VM state to disk as JSON files.
// It wraps a MemoryStore for in-memory lookups and mirrors every mutation
// to ~/.uni/vms/<id>/state.json.
type FileStore struct {
	MemoryStore
	root string // e.g. ~/.uni/vms
	mu   sync.Mutex
}

// NewFileStore returns a FileStore rooted at dir (~/.uni/vms).
// Call Restore() to load any previously persisted VMs.
func NewFileStore(dir string) *FileStore {
	return &FileStore{
		MemoryStore: *NewMemoryStore(),
		root:        dir,
	}
}

func (s *FileStore) Create(cfg Config) (*VM, error) {
	v, err := s.MemoryStore.Create(cfg)
	if err != nil {
		return nil, err
	}
	if err := s.writeState(v); err != nil {
		_ = s.MemoryStore.Remove(v.ID)
		return nil, fmt.Errorf("persist vm %s: %w", v.ID, err)
	}
	return v, nil
}

func (s *FileStore) Remove(id string) error {
	if err := s.MemoryStore.Remove(id); err != nil {
		return err
	}
	dir := filepath.Join(s.root, id)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove vm dir %s: %w", id, err)
	}
	return nil
}

func (s *FileStore) Save(v *VM) error {
	return s.writeState(v)
}

// Restore loads all persisted VMs from disk into memory. VMs that were
// in the "running" or "starting" state are marked as "stopped" with
// DaemonRecovered=true, since the QEMU process died with the daemon.
func (s *FileStore) Restore() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("restore: read vms dir: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.root, e.Name(), "state.json"))
		if err != nil {
			slog.Warn("restore: skip vm", "id", e.Name(), "err", err)
			continue
		}
		var st vmState
		if err := json.Unmarshal(data, &st); err != nil {
			slog.Warn("restore: parse vm state", "id", e.Name(), "err", err)
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
		if st.HealthStatus != "" {
			v.HealthStatus = HealthStatus(st.HealthStatus)
		}

		switch st.State {
		case StateRunning, StateStarting:
			slog.Info("restore: marking vm as stopped (daemon restart)", "vm_id", v.ID)
			v.State = StateStopped
			now := time.Now()
			v.StoppedAt = &now
			v.DaemonRecovered = true
			close(v.done)
		case StateStopped:
			close(v.done)
		default:
			// StateCreated or unknown: keep as-is.
		}

		s.vms[v.ID] = v
	}
	return nil
}

func (s *FileStore) writeState(v *VM) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	v.mu.RLock()
	st := vmState{
		ID:              v.ID,
		Config:          v.Cfg,
		State:           v.State,
		CreatedAt:       v.CreatedAt,
		StartedAt:       v.StartedAt,
		StoppedAt:       v.StoppedAt,
		DaemonRecovered: v.DaemonRecovered,
		HealthStatus:    string(v.HealthStatus),
		RestartCount:    v.RestartCount,
	}
	v.mu.RUnlock()

	dir := filepath.Join(s.root, v.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create vm dir: %w", err)
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal vm state: %w", err)
	}

	path := filepath.Join(dir, "state.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write vm state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename vm state: %w", err)
	}
	return nil
}
