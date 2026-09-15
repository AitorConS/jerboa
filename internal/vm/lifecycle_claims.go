//go:build linux || (darwin && arm64)

package vm

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/AitorConS/jerboa/internal/snapshot"
)

var (
	// ErrVMBusy is returned when another lifecycle operation owns the VM.
	ErrVMBusy = errors.New("vm lifecycle operation in progress")
	// ErrSnapshotUnsupported marks a backend or configuration without
	// snapshot support; the API reports it as UNSUPPORTED_CAPABILITY.
	ErrSnapshotUnsupported = errors.New("UNSUPPORTED_CAPABILITY")
)

const (
	opSnapshotCreate  = "snapshot-create"
	opSnapshotRestore = "snapshot-restore"
	opRestart         = "restart"
)

// lifecycleClaims gives one long-running lifecycle operation exclusive
// ownership of a VM. All lifecycle operations acquire it, and automatic restart both
// advertises its backoff and claims the VM before creating a replacement.
type lifecycleClaims struct {
	mu      sync.Mutex
	ops     map[string]string
	restart map[string]bool
}

func (c *lifecycleClaims) claim(id, op string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.ops[id]; ok {
		return fmt.Errorf("%w: %s", ErrVMBusy, cur)
	}
	if c.ops == nil {
		c.ops = map[string]string{}
	}
	c.ops[id] = op
	return nil
}

func (c *lifecycleClaims) release(id, op string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ops[id] == op {
		delete(c.ops, id)
	}
}

func (c *lifecycleClaims) current(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ops[id]
}

func (c *lifecycleClaims) setRestartPending(id string, pending bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !pending {
		delete(c.restart, id)
		return
	}
	if c.restart == nil {
		c.restart = map[string]bool{}
	}
	c.restart[id] = true
}

func (c *lifecycleClaims) restartPending(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restart[id]
}

// WithFCSnapshotStore enables snapshots backed by the daemon-private store.
func WithFCSnapshotStore(s *snapshot.Store) FCOption {
	return func(m *FirecrackerManager) { m.snapshots = s }
}

// claimVM also rechecks membership after claiming: Remove may have completed
// between Resolve and claim, leaving the caller with a stale VM pointer.
func (m *FirecrackerManager) claimVM(id, op string) (*VM, func(), error) {
	v, err := m.store.Resolve(id)
	if err != nil {
		return nil, nil, err
	}
	if err := m.claims.claim(v.ID, op); err != nil {
		return nil, nil, err
	}
	release := func() { m.claims.release(v.ID, op) }
	current, err := m.store.Get(v.ID)
	if err != nil || current != v {
		release()
		return nil, nil, fmt.Errorf("vm %s was removed or replaced", v.ID)
	}
	if v.GetState() == StateRestoring {
		release()
		return nil, nil, fmt.Errorf("%w: %s", ErrVMBusy, opSnapshotRestore)
	}
	return v, release, nil
}

// beginRestore moves a stopped VM to restoring, renewing the done channel the
// previous incarnation closed and clearing its process and stop markers.
func (v *VM) beginRestore() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.State != StateStopped {
		return fmt.Errorf("%w %s → %s", ErrInvalidTransition, v.State, StateRestoring)
	}
	v.State = StateRestoring
	v.done = make(chan struct{})
	v.explicitStop = false
	v.DaemonRecovered = false
	v.proc = nil
	v.pid = 0
	slog.Info("vm state transition", "vm_id", v.ID, "from", StateStopped, "to", StateRestoring)
	return nil
}
