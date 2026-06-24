//go:build linux

package vm

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// Store is the interface for a VM registry. Implementations may persist
// state to disk (FileStore) or keep it in memory only (MemoryStore).
type Store interface {
	Create(cfg Config) (*VM, error)
	Get(id string) (*VM, error)
	Resolve(nameOrID string) (*VM, error)
	List() []*VM
	Remove(id string) error
	Save(v *VM) error
	Restore() error
}

// MemoryStore is a thread-safe in-memory registry of VMs.
type MemoryStore struct {
	mu  sync.RWMutex
	vms map[string]*VM
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{vms: make(map[string]*VM)}
}

// NewStore returns an empty in-memory Store.
//
// Deprecated: use NewMemoryStore for clarity.
func NewStore() *MemoryStore {
	return NewMemoryStore()
}

func (s *MemoryStore) Create(cfg Config) (*VM, error) {
	id, err := newID()
	if err != nil {
		return nil, fmt.Errorf("create vm: generate id: %w", err)
	}
	v := &VM{
		ID:        id,
		Cfg:       cfg,
		State:     StateCreated,
		CreatedAt: time.Now(),
		done:      make(chan struct{}),
	}
	s.mu.Lock()
	s.vms[id] = v
	s.mu.Unlock()
	return v, nil
}

func (s *MemoryStore) Get(id string) (*VM, error) {
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("vm %s not found", id)
	}
	return v, nil
}

func (s *MemoryStore) Resolve(nameOrID string) (*VM, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if v, ok := s.vms[nameOrID]; ok {
		return v, nil
	}

	for _, v := range s.vms {
		if v.Cfg.Name == nameOrID {
			return v, nil
		}
	}

	var matched *VM
	for id, v := range s.vms {
		if len(nameOrID) <= len(id) && id[:len(nameOrID)] == nameOrID {
			if matched != nil {
				return nil, fmt.Errorf("vm %q is ambiguous (matches multiple IDs)", nameOrID)
			}
			matched = v
		}
	}
	if matched != nil {
		return matched, nil
	}

	return nil, fmt.Errorf("vm %q not found", nameOrID)
}

func (s *MemoryStore) List() []*VM {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*VM, 0, len(s.vms))
	for _, v := range s.vms {
		out = append(out, v)
	}
	return out
}

func (s *MemoryStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.vms[id]; !ok {
		return fmt.Errorf("vm %q not found", id)
	}
	delete(s.vms, id)
	return nil
}

// Save is a no-op for MemoryStore; state lives only in memory.
func (s *MemoryStore) Save(_ *VM) error { return nil }

// Restore is a no-op for MemoryStore; there is nothing to restore from.
func (s *MemoryStore) Restore() error { return nil }

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
