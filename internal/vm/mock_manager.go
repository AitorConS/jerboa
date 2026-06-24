//go:build linux

package vm

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

type MockManager struct {
	mu       sync.RWMutex
	vms      map[string]*VM
	nextID   int
	CreateFn func(ctx context.Context, cfg Config) (*VM, error)
	StartFn  func(ctx context.Context, id string) error
	StopFn   func(ctx context.Context, id string) error
	KillFn   func(ctx context.Context, id string) error
	SignalFn func(ctx context.Context, id string, sig os.Signal) error
	RemoveFn func(ctx context.Context, id string) error
	GetFn    func(id string) (*VM, error)
	ListFn   func() []*VM
}

func NewMockManager() *MockManager {
	return &MockManager{
		vms: make(map[string]*VM),
	}
}

func (m *MockManager) Create(ctx context.Context, cfg Config) (*VM, error) {
	if m.CreateFn != nil {
		return m.CreateFn(ctx, cfg)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("mock%06x", m.nextID)
	v := &VM{
		ID:        id,
		Cfg:       cfg,
		State:     StateCreated,
		CreatedAt: time.Now(),
		done:      make(chan struct{}),
	}
	m.vms[id] = v
	return v, nil
}

func (m *MockManager) Start(ctx context.Context, id string) error {
	if m.StartFn != nil {
		return m.StartFn(ctx, id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return fmt.Errorf("mock manager: vm %s not found", id)
	}
	if err := v.transition(StateStarting); err != nil {
		return err
	}
	return v.transition(StateRunning)
}

func (m *MockManager) Stop(ctx context.Context, id string) error {
	if m.StopFn != nil {
		return m.StopFn(ctx, id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return fmt.Errorf("mock manager: vm %s not found", id)
	}
	if err := v.transition(StateStopping); err != nil {
		return err
	}
	return v.transition(StateStopped)
}

func (m *MockManager) Kill(ctx context.Context, id string) error {
	if m.KillFn != nil {
		return m.KillFn(ctx, id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return fmt.Errorf("mock manager: vm %s not found", id)
	}
	_ = v.transition(StateStopping)
	return v.transition(StateStopped)
}

func (m *MockManager) Signal(ctx context.Context, id string, sig os.Signal) error {
	if m.SignalFn != nil {
		return m.SignalFn(ctx, id, sig)
	}
	return nil
}

func (m *MockManager) Remove(ctx context.Context, id string) error {
	if m.RemoveFn != nil {
		return m.RemoveFn(ctx, id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return fmt.Errorf("mock manager: vm %s not found", id)
	}
	if v.GetState() != StateStopped {
		return fmt.Errorf("mock manager: vm %s is %s, must be stopped", id, v.GetState())
	}
	delete(m.vms, id)
	return nil
}

func (m *MockManager) Get(id string) (*VM, error) {
	if m.GetFn != nil {
		return m.GetFn(id)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.vms[id]
	if !ok {
		return nil, fmt.Errorf("mock manager: vm %s not found", id)
	}
	return v, nil
}

func (m *MockManager) List() []*VM {
	if m.ListFn != nil {
		return m.ListFn()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*VM, 0, len(m.vms))
	for _, v := range m.vms {
		out = append(out, v)
	}
	return out
}
