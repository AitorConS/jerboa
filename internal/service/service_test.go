package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/compose"
	"github.com/AitorConS/unikernel-engine/internal/vm"
	"github.com/stretchr/testify/require"
)

type mockStore struct {
	mu   sync.Mutex
	svcs map[string]*Service
}

func newMockStore() *mockStore {
	return &mockStore{svcs: make(map[string]*Service)}
}

func (s *mockStore) Save(svc *Service) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.svcs[svc.Name] = svc
	return nil
}

func (s *mockStore) Get(name string) (*Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.svcs[name]
	if !ok {
		return nil, fmt.Errorf("service %q not found", name)
	}
	return svc, nil
}

func (s *mockStore) List() ([]*Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Service
	for _, svc := range s.svcs {
		out = append(out, svc)
	}
	return out, nil
}

func (s *mockStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.svcs, name)
	return nil
}

func TestServiceRun(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	svc, err := svcMgr.Run(context.Background(), "web", "nginx:latest", 2, Options{Memory: "256M"})
	require.NoError(t, err)
	require.Equal(t, "web", svc.Name)
	require.Equal(t, "nginx:latest", svc.Image)
	require.Equal(t, 2, svc.DesiredReplicas)
	require.Equal(t, StrategyRollingUpdate, svc.Strategy)

	replicas := svcMgr.Replicas("web")
	require.Len(t, replicas, 2)

	saved, err := store.Get("web")
	require.NoError(t, err)
	require.Equal(t, "web", saved.Name)
}

func TestServiceRunDefaults(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	svc, err := svcMgr.Run(context.Background(), "web", "nginx:latest", 1, Options{})
	require.NoError(t, err)
	require.Equal(t, "256M", svc.Config.Memory)
	require.Equal(t, 1, svc.Config.CPUs)
}

func TestServiceRunDuplicate(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Run(context.Background(), "web", "nginx:latest", 1, Options{})
	require.NoError(t, err)
	_, err = svcMgr.Run(context.Background(), "web", "nginx:latest", 1, Options{})
	require.Error(t, err)
}

func TestServiceRunValidation(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Run(context.Background(), "", "nginx:latest", 1, Options{})
	require.Error(t, err)

	_, err = svcMgr.Run(context.Background(), "web", "", 1, Options{})
	require.Error(t, err)

	_, err = svcMgr.Run(context.Background(), "web", "nginx:latest", 0, Options{})
	require.Error(t, err)
}

func TestServiceScaleUp(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Run(context.Background(), "web", "nginx:latest", 1, Options{})
	require.NoError(t, err)

	svc, err := svcMgr.Scale(context.Background(), "web", 3)
	require.NoError(t, err)
	require.Equal(t, 3, svc.DesiredReplicas)

	replicas := svcMgr.Replicas("web")
	require.Len(t, replicas, 3)
}

func TestServiceScaleDown(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Run(context.Background(), "web", "nginx:latest", 3, Options{})
	require.NoError(t, err)

	svc, err := svcMgr.Scale(context.Background(), "web", 1)
	require.NoError(t, err)
	require.Equal(t, 1, svc.DesiredReplicas)
}

func TestServiceScaleNotFound(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Scale(context.Background(), "nope", 3)
	require.Error(t, err)
}

func TestServiceScaleNegative(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Scale(context.Background(), "web", -1)
	require.Error(t, err)
}

func TestServiceRemove(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Run(context.Background(), "web", "nginx:latest", 2, Options{})
	require.NoError(t, err)

	err = svcMgr.Remove(context.Background(), "web")
	require.NoError(t, err)

	_, err = store.Get("web")
	require.Error(t, err)

	replicas := svcMgr.Replicas("web")
	require.Len(t, replicas, 0)
}

func TestServiceRemoveNotFound(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	err := svcMgr.Remove(context.Background(), "nope")
	require.Error(t, err)
}

func TestServiceList(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Run(context.Background(), "web1", "nginx:latest", 1, Options{})
	require.NoError(t, err)
	_, err = svcMgr.Run(context.Background(), "web2", "redis:latest", 2, Options{})
	require.NoError(t, err)

	services, err := svcMgr.List()
	require.NoError(t, err)
	require.Len(t, services, 2)
}

func TestServiceGet(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Run(context.Background(), "web", "nginx:latest", 1, Options{})
	require.NoError(t, err)

	svc, err := svcMgr.Get("web")
	require.NoError(t, err)
	require.Equal(t, "web", svc.Name)
}

func TestServiceGetNotFound(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Get("nope")
	require.Error(t, err)
}

func TestServiceUpdateRolling(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Run(context.Background(), "web", "nginx:v1", 1, Options{})
	require.NoError(t, err)

	svc, err := svcMgr.Update(context.Background(), "web", "nginx:v2", 0)
	require.NoError(t, err)
	require.Equal(t, "nginx:v2", svc.Image)
}

func TestServiceUpdateRecreate(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	opts := Options{Strategy: StrategyRecreate}
	svc, err := svcMgr.Run(context.Background(), "web", "nginx:v1", 1, opts)
	require.NoError(t, err)
	require.Equal(t, StrategyRecreate, svc.Strategy)

	svc, err = svcMgr.Update(context.Background(), "web", "nginx:v2", 0)
	require.NoError(t, err)
	require.Equal(t, "nginx:v2", svc.Image)
}

func TestServiceUpdateNotFound(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Update(context.Background(), "nope", "nginx:v2", 0)
	require.Error(t, err)
}

func TestInfo(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	svc, err := svcMgr.Run(context.Background(), "web", "nginx:latest", 2, Options{})
	require.NoError(t, err)

	info := svcMgr.ServiceInfo(svc)
	require.Equal(t, "web", info.Name)
	require.Equal(t, "nginx:latest", info.Image)
	require.Equal(t, 2, info.DesiredReplicas)
	require.Equal(t, string(StrategyRollingUpdate), info.Strategy)
	require.Len(t, info.ReplicaIDs, 2)
}

func TestAggregateHealth(t *testing.T) {
	tests := []struct {
		name     string
		replicas []*vm.VM
		want     vm.HealthStatus
	}{
		{name: "empty", replicas: nil, want: vm.HealthUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aggregateHealth(tt.replicas)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestReplicaName(t *testing.T) {
	require.Equal(t, "web-0", replicaName("web", 0))
	require.Equal(t, "web-3", replicaName("web", 3))
	require.Equal(t, "myapp-10", replicaName("myapp", 10))
}

func TestFileStore(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)

	svc := &Service{
		Name:            "web",
		Image:           "nginx:latest",
		DesiredReplicas: 3,
		Strategy:        StrategyRollingUpdate,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}

	err = store.Save(svc)
	require.NoError(t, err)

	got, err := store.Get("web")
	require.NoError(t, err)
	require.Equal(t, "web", got.Name)
	require.Equal(t, "nginx:latest", got.Image)
	require.Equal(t, 3, got.DesiredReplicas)

	services, err := store.List()
	require.NoError(t, err)
	require.Len(t, services, 1)

	err = store.Delete("web")
	require.NoError(t, err)

	_, err = store.Get("web")
	require.Error(t, err)
}

func TestFileStoreGetNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)

	_, err = store.Get("nope")
	require.Error(t, err)
}

func TestFileStoreDeleteIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	require.NoError(t, err)

	err = store.Delete("nonexistent")
	require.NoError(t, err)
}

func TestServiceRunWithHealthTimeout(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	opts := Options{HealthTimeout: 2 * time.Second}
	svc, err := svcMgr.Run(context.Background(), "web", "nginx:latest", 1, opts)
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, svc.HealthTimeout)
}

func TestServiceUpdateWithHealthTimeout(t *testing.T) {
	mgr := vm.NewMockManager()
	store := newMockStore()
	svcMgr := NewManager(mgr, store)

	_, err := svcMgr.Run(context.Background(), "web", "nginx:v1", 1, Options{})
	require.NoError(t, err)

	svc, err := svcMgr.Update(context.Background(), "web", "nginx:v2", 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "nginx:v2", svc.Image)
}

func TestComposeServiceReplicas(t *testing.T) {
	data := []byte(`
version: "1"
services:
  web:
    image: nginx:latest
    replicas: 3
    strategy: RollingUpdate
networks: {}
`)
	f, err := compose.Parse(data)
	require.NoError(t, err)
	require.Equal(t, 3, f.Services["web"].Replicas)
	require.Equal(t, "RollingUpdate", f.Services["web"].Strategy)
}

func TestComposeServiceReplicasValidation(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  web:
    image: nginx:latest
    replicas: -1
networks: {}
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "replicas must be non-negative")
}

func TestComposeServiceStrategyValidation(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  web:
    image: nginx:latest
    strategy: Invalid
networks: {}
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "strategy must be RollingUpdate or Recreate")
}
