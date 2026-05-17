package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/vm"
)

// Strategy determines how updates are applied to a service's replicas.
type Strategy string

const (
	// StrategyRollingUpdate creates new replicas before removing old ones.
	StrategyRollingUpdate Strategy = "RollingUpdate"
	// StrategyRecreate stops all replicas before starting new ones.
	StrategyRecreate Strategy = "Recreate"
)

// Service represents a group of VM replicas managed as a single unit.
type Service struct {
	// Name is the unique service identifier.
	Name string
	// Image is the unikernel image reference (e.g. "myapp:latest").
	Image string
	// DesiredReplicas is the target number of running replicas.
	DesiredReplicas int
	// Strategy controls how updates are applied.
	Strategy Strategy
	// HealthTimeout is the maximum duration to wait for new replicas to
	// become healthy during rolling updates. Zero means no waiting.
	HealthTimeout time.Duration
	// Config holds the VM configuration template used for each replica.
	Config vm.Config
	// CreatedAt is the timestamp when the service was created.
	CreatedAt time.Time
	// UpdatedAt is the timestamp when the service was last updated.
	UpdatedAt time.Time
}

// Info is the serialisable representation of a service for API responses.
type Info struct {
	Name            string   `json:"name"`
	Image           string   `json:"image"`
	DesiredReplicas int      `json:"desired_replicas"`
	ReadyReplicas   int      `json:"ready_replicas"`
	Strategy        string   `json:"strategy"`
	Health          string   `json:"health"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	ReplicaIDs      []string `json:"replica_ids"`
}

// ReplicaInfo describes a single replica within a service.
type ReplicaInfo struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	State  vm.State        `json:"state"`
	Health vm.HealthStatus `json:"health"`
	IP     string          `json:"ip,omitempty"`
}

// Options contains optional parameters for creating a service.
type Options struct {
	Memory        string
	CPUs          int
	Env           []string
	PortMaps      []vm.PortMap
	NetworkName   string
	HealthCheck   *vm.HealthCheckConfig
	Restart       vm.RestartConfig
	Strategy      Strategy
	HealthTimeout time.Duration
}

// aggregateHealth computes the overall health of a service from its replicas.
func aggregateHealth(replicas []*vm.VM) vm.HealthStatus {
	if len(replicas) == 0 {
		return vm.HealthUnknown
	}
	healthy := 0
	unhealthy := 0
	for _, r := range replicas {
		switch r.GetHealthStatus() {
		case vm.HealthHealthy:
			healthy++
		case vm.HealthUnhealthy:
			unhealthy++
		}
	}
	if healthy == len(replicas) {
		return vm.HealthHealthy
	}
	if unhealthy == len(replicas) {
		return vm.HealthUnhealthy
	}
	if healthy > 0 {
		return vm.HealthStarting
	}
	return vm.HealthUnknown
}

// replicaName generates a replica VM name from the service name and index.
func replicaName(serviceName string, index int) string {
	return fmt.Sprintf("%s-%d", serviceName, index)
}

// Manager manages service lifecycle: creation, scaling, updates, and removal.
type Manager struct {
	mgr   vm.Manager
	store StoreInterface
	mu    sync.RWMutex
}

// StoreInterface is the interface for persisting and retrieving services.
type StoreInterface interface {
	Save(svc *Service) error
	Get(name string) (*Service, error)
	List() ([]*Service, error)
	Delete(name string) error
}

// NewManager creates a new service Manager backed by the given VM manager and store.
func NewManager(mgr vm.Manager, store StoreInterface) *Manager {
	return &Manager{
		mgr:   mgr,
		store: store,
	}
}
