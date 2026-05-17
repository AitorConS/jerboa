package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/vm"
)

// Run creates a service with the desired number of replicas and starts them.
func (m *Manager) Run(ctx context.Context, name, image string, replicas int, opts Options) (*Service, error) {
	if name == "" {
		return nil, fmt.Errorf("service run: name is required")
	}
	if image == "" {
		return nil, fmt.Errorf("service run: image is required")
	}
	if replicas < 1 {
		return nil, fmt.Errorf("service run: replicas must be at least 1, got %d", replicas)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, err := m.store.Get(name)
	if err == nil && existing != nil {
		return nil, fmt.Errorf("service run: service %q already exists", name)
	}

	cfg := vm.Config{
		ImagePath:   image,
		Memory:      opts.Memory,
		CPUs:        opts.CPUs,
		Env:         opts.Env,
		PortMaps:    opts.PortMaps,
		NetworkName: opts.NetworkName,
		HealthCheck: opts.HealthCheck,
		Restart:     opts.Restart,
	}

	if cfg.Memory == "" {
		cfg.Memory = "256M"
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}

	strategy := opts.Strategy
	if strategy == "" {
		strategy = StrategyRollingUpdate
	}

	svc := &Service{
		Name:            name,
		Image:           image,
		DesiredReplicas: replicas,
		Strategy:        strategy,
		HealthTimeout:   opts.HealthTimeout,
		Config:          cfg,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}

	createdVMs := make([]*vm.VM, 0, replicas)
	for i := 0; i < replicas; i++ {
		replicaCfg := cfg
		replicaCfg.Name = replicaName(name, i)

		v, err := m.mgr.Create(ctx, replicaCfg)
		if err != nil {
			for _, created := range createdVMs {
				_ = m.mgr.Remove(ctx, created.ID)
			}
			return nil, fmt.Errorf("service run: create replica %d: %w", i, err)
		}

		if err := m.mgr.Start(ctx, v.ID); err != nil {
			_ = m.mgr.Remove(ctx, v.ID)
			return nil, fmt.Errorf("service run: start replica %d: %w", i, err)
		}

		createdVMs = append(createdVMs, v)
		slog.Info("service replica started", "service", name, "replica", replicaCfg.Name, "vm_id", v.ID)
	}

	if svc.HealthTimeout > 0 {
		if err := m.waitForReplicasHealthy(ctx, createdVMs, svc.HealthTimeout); err != nil {
			slog.Warn("service run: health check timeout", "service", name, "error", err)
		}
	}

	if err := m.store.Save(svc); err != nil {
		for _, created := range createdVMs {
			_ = m.mgr.Stop(ctx, created.ID)
			_ = m.mgr.Remove(ctx, created.ID)
		}
		return nil, fmt.Errorf("service run: save service: %w", err)
	}

	return svc, nil
}

// Scale adjusts the number of replicas for a service to the desired count.
func (m *Manager) Scale(ctx context.Context, name string, desiredReplicas int) (*Service, error) {
	if desiredReplicas < 0 {
		return nil, fmt.Errorf("service scale: replicas must be non-negative, got %d", desiredReplicas)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	svc, err := m.store.Get(name)
	if err != nil {
		return nil, fmt.Errorf("service scale: service %q not found", name)
	}

	currentReplicas := m.replicaVMs(name)
	currentCount := len(currentReplicas)

	switch {
	case desiredReplicas > currentCount:
		err = m.scaleUp(ctx, svc, currentCount, desiredReplicas)
	case desiredReplicas < currentCount:
		err = m.scaleDown(ctx, svc, currentReplicas, desiredReplicas)
	default:
		slog.Info("service already at desired replicas", "service", name, "replicas", desiredReplicas)
	}

	if err != nil {
		return nil, err
	}

	svc.DesiredReplicas = desiredReplicas
	svc.UpdatedAt = time.Now().UTC()
	if saveErr := m.store.Save(svc); saveErr != nil {
		return nil, fmt.Errorf("service scale: save: %w", saveErr)
	}

	return svc, nil
}

func (m *Manager) scaleUp(ctx context.Context, svc *Service, currentCount, desiredReplicas int) error {
	for i := currentCount; i < desiredReplicas; i++ {
		replicaCfg := svc.Config
		replicaCfg.Name = replicaName(svc.Name, i)

		v, err := m.mgr.Create(ctx, replicaCfg)
		if err != nil {
			return fmt.Errorf("service scale up: create replica %d: %w", i, err)
		}

		if err := m.mgr.Start(ctx, v.ID); err != nil {
			_ = m.mgr.Remove(ctx, v.ID)
			return fmt.Errorf("service scale up: start replica %d: %w", i, err)
		}

		slog.Info("service scale up: replica started", "service", svc.Name, "replica", replicaCfg.Name, "vm_id", v.ID)
	}
	return nil
}

func (m *Manager) scaleDown(ctx context.Context, svc *Service, currentReplicas []*vm.VM, desiredReplicas int) error {
	for i := len(currentReplicas) - 1; i >= desiredReplicas; i-- {
		v := currentReplicas[i]
		if err := m.mgr.Stop(ctx, v.ID); err != nil {
			slog.Warn("service scale down: stop replica failed", "vm_id", v.ID, "error", err)
			_ = m.mgr.Kill(ctx, v.ID)
		}
		_ = m.mgr.Remove(ctx, v.ID)
		slog.Info("service scale down: replica removed", "service", svc.Name, "vm_id", v.ID)
	}
	return nil
}

// Update performs a rolling update of the service to a new image.
// If healthTimeout > 0, it waits for new replicas to become healthy before
// removing old ones.
func (m *Manager) Update(ctx context.Context, name, newImage string, healthTimeout time.Duration) (*Service, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	svc, err := m.store.Get(name)
	if err != nil {
		return nil, fmt.Errorf("service update: service %q not found", name)
	}

	timeout := healthTimeout
	if timeout == 0 {
		timeout = svc.HealthTimeout
	}

	if svc.Strategy == StrategyRecreate {
		return m.recreateUpdate(ctx, svc, newImage, timeout)
	}
	return m.rollingUpdate(ctx, svc, newImage, timeout)
}

// rollingUpdate creates new replicas, waits for them to become healthy (if
// healthTimeout > 0), then removes old replicas.
func (m *Manager) rollingUpdate(ctx context.Context, svc *Service, newImage string, healthTimeout time.Duration) (*Service, error) {
	oldReplicas := m.replicaVMs(svc.Name)

	newCfg := svc.Config
	newCfg.ImagePath = newImage

	var newReplicas []*vm.VM
	for i := 0; i < svc.DesiredReplicas; i++ {
		replicaCfg := newCfg
		replicaCfg.Name = replicaName(svc.Name, i)

		v, err := m.mgr.Create(ctx, replicaCfg)
		if err != nil {
			return nil, fmt.Errorf("service update: create replica %d: %w", i, err)
		}

		if err := m.mgr.Start(ctx, v.ID); err != nil {
			_ = m.mgr.Remove(ctx, v.ID)
			return nil, fmt.Errorf("service update: start replica %d: %w", i, err)
		}
		newReplicas = append(newReplicas, v)

		slog.Info("service update: new replica started", "service", svc.Name, "replica", replicaCfg.Name, "vm_id", v.ID, "image", newImage)
	}

	if healthTimeout > 0 {
		if err := m.waitForReplicasHealthy(ctx, newReplicas, healthTimeout); err != nil {
			slog.Warn("service update: health check timeout, proceeding with old replica removal", "service", svc.Name, "error", err)
		}
	}

	for _, old := range oldReplicas {
		if err := m.mgr.Stop(ctx, old.ID); err != nil {
			slog.Warn("service update: stop old replica failed", "vm_id", old.ID, "error", err)
			_ = m.mgr.Kill(ctx, old.ID)
		}
		_ = m.mgr.Remove(ctx, old.ID)
		slog.Info("service update: old replica removed", "service", svc.Name, "vm_id", old.ID)
	}

	svc.Image = newImage
	svc.UpdatedAt = time.Now().UTC()
	if err := m.store.Save(svc); err != nil {
		return nil, fmt.Errorf("service update: save: %w", err)
	}

	return svc, nil
}

// recreateUpdate stops all replicas, then starts new ones with the updated image.
// If healthTimeout > 0, it waits for new replicas to become healthy after starting.
func (m *Manager) recreateUpdate(ctx context.Context, svc *Service, newImage string, healthTimeout time.Duration) (*Service, error) {
	oldReplicas := m.replicaVMs(svc.Name)

	for _, v := range oldReplicas {
		if err := m.mgr.Stop(ctx, v.ID); err != nil {
			slog.Warn("service update (recreate): stop replica failed", "vm_id", v.ID, "error", err)
			_ = m.mgr.Kill(ctx, v.ID)
		}
		_ = m.mgr.Remove(ctx, v.ID)
	}

	newCfg := svc.Config
	newCfg.ImagePath = newImage

	var newReplicas []*vm.VM
	for i := 0; i < svc.DesiredReplicas; i++ {
		replicaCfg := newCfg
		replicaCfg.Name = replicaName(svc.Name, i)

		v, err := m.mgr.Create(ctx, replicaCfg)
		if err != nil {
			return nil, fmt.Errorf("service update (recreate): create replica %d: %w", i, err)
		}

		if err := m.mgr.Start(ctx, v.ID); err != nil {
			_ = m.mgr.Remove(ctx, v.ID)
			return nil, fmt.Errorf("service update (recreate): start replica %d: %w", i, err)
		}
		newReplicas = append(newReplicas, v)
	}

	if healthTimeout > 0 {
		if err := m.waitForReplicasHealthy(ctx, newReplicas, healthTimeout); err != nil {
			slog.Warn("service update (recreate): health check timeout", "service", svc.Name, "error", err)
		}
	}

	svc.Image = newImage
	svc.UpdatedAt = time.Now().UTC()
	if err := m.store.Save(svc); err != nil {
		return nil, fmt.Errorf("service update (recreate): save: %w", err)
	}

	return svc, nil
}

// Get returns a service by name.
func (m *Manager) Get(name string) (*Service, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	svc, err := m.store.Get(name)
	if err != nil {
		return nil, fmt.Errorf("service get: %w", err)
	}
	return svc, nil
}

// List returns all services.
func (m *Manager) List() ([]*Service, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.List()
}

// Remove stops all replicas of a service and deletes it.
func (m *Manager) Remove(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.store.Get(name); err != nil {
		return fmt.Errorf("service remove: service %q not found", name)
	}

	replicas := m.replicaVMs(name)
	for _, v := range replicas {
		if err := m.mgr.Stop(ctx, v.ID); err != nil {
			slog.Warn("service remove: stop replica failed", "vm_id", v.ID, "error", err)
			_ = m.mgr.Kill(ctx, v.ID)
		}
		_ = m.mgr.Remove(ctx, v.ID)
		slog.Info("service remove: replica removed", "service", name, "vm_id", v.ID)
	}

	if err := m.store.Delete(name); err != nil {
		return fmt.Errorf("service remove: delete: %w", err)
	}

	return nil
}

// Replicas returns the VM replicas belonging to a service.
func (m *Manager) Replicas(name string) []*vm.VM {
	return m.replicaVMs(name)
}

// ServiceInfo builds an Info from a Service and its live replicas.
func (m *Manager) ServiceInfo(svc *Service) Info {
	replicas := m.replicaVMs(svc.Name)
	health := aggregateHealth(replicas)
	ready := 0
	replicaIDs := make([]string, 0, len(replicas))
	for _, r := range replicas {
		replicaIDs = append(replicaIDs, r.ID)
		if r.GetState() == vm.StateRunning && r.GetHealthStatus() == vm.HealthHealthy {
			ready++
		}
	}

	strategy := string(svc.Strategy)
	if strategy == "" {
		strategy = string(StrategyRollingUpdate)
	}

	return Info{
		Name:            svc.Name,
		Image:           svc.Image,
		DesiredReplicas: svc.DesiredReplicas,
		ReadyReplicas:   ready,
		Strategy:        strategy,
		Health:          string(health),
		CreatedAt:       svc.CreatedAt.Format(time.RFC3339),
		UpdatedAt:       svc.UpdatedAt.Format(time.RFC3339),
		ReplicaIDs:      replicaIDs,
	}
}

// replicaVMs finds all VMs belonging to a service by name prefix.
func (m *Manager) replicaVMs(serviceName string) []*vm.VM {
	allVMs := m.mgr.List()
	prefix := serviceName + "-"
	var result []*vm.VM
	for _, v := range allVMs {
		if v.Cfg.Name != "" && len(v.Cfg.Name) > len(prefix) && v.Cfg.Name[:len(prefix)] == prefix {
			result = append(result, v)
		}
	}
	return result
}

// healthCheckInterval is the polling interval when waiting for replicas
// to become healthy.
const healthCheckInterval = 500 * time.Millisecond

// waitForReplicasHealthy polls the given VMs until all are healthy or the
// timeout expires. Returns an error if the timeout is reached before all
// replicas report healthy. Returns nil immediately if no health check is
// configured on the VMs.
func (m *Manager) waitForReplicasHealthy(ctx context.Context, vms []*vm.VM, timeout time.Duration) error {
	if timeout <= 0 || len(vms) == 0 {
		return nil
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for healthy replicas: %w", ctx.Err())
		default:
		}

		allHealthy := true
		for _, v := range vms {
			v, err := m.mgr.Get(v.ID)
			if err != nil {
				allHealthy = false
				break
			}
			if v.GetHealthStatus() != vm.HealthHealthy {
				allHealthy = false
				break
			}
		}
		if allHealthy {
			return nil
		}
		time.Sleep(healthCheckInterval)
	}
	return fmt.Errorf("timed out waiting for replicas to become healthy after %v", timeout)
}
