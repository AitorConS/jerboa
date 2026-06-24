//go:build linux

package vm

import (
	"context"
	"os"
)

// Manager manages the lifecycle of unikernel VMs.
type Manager interface {
	// Create registers a new VM with the given config.
	Create(ctx context.Context, cfg Config) (*VM, error)
	// Start launches the QEMU process for the VM with the given id.
	Start(ctx context.Context, id string) error
	// Stop gracefully shuts down the VM: SIGTERM, 30s grace, then SIGKILL.
	Stop(ctx context.Context, id string) error
	// Kill immediately sends SIGKILL to the VM process.
	Kill(ctx context.Context, id string) error
	// Signal sends an arbitrary OS signal to the VM process.
	Signal(ctx context.Context, id string, sig os.Signal) error
	// Remove deletes a stopped VM from the registry.
	Remove(ctx context.Context, id string) error
	// Get returns the VM with the given id.
	Get(id string) (*VM, error)
	// List returns all registered VMs.
	List() []*VM
}
