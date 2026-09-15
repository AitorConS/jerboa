//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLifecycleClaimsExclusive(t *testing.T) {
	var c lifecycleClaims
	require.NoError(t, c.claim("a", opSnapshotRestore))
	require.ErrorIs(t, c.claim("a", opSnapshotCreate), ErrVMBusy)
	require.NoError(t, c.claim("b", opSnapshotCreate))
	c.release("a", opSnapshotCreate)
	require.Equal(t, opSnapshotRestore, c.current("a"), "release by a different op is ignored")
	c.release("a", opSnapshotRestore)
	require.Empty(t, c.current("a"))

	require.False(t, c.restartPending("a"))
	c.setRestartPending("a", true)
	require.True(t, c.restartPending("a"))
	c.setRestartPending("a", false)
	require.False(t, c.restartPending("a"))
}

func TestBeginRestoreRenewsDone(t *testing.T) {
	v := &VM{ID: "0123456789ab", State: StateStopped, done: make(chan struct{})}
	close(v.done)
	v.explicitStop = true
	v.DaemonRecovered = true
	require.NoError(t, v.beginRestore())
	require.Equal(t, StateRestoring, v.GetState())
	require.False(t, v.IsExplicitStop())
	require.False(t, v.DaemonRecovered)
	select {
	case <-v.Done():
		t.Fatal("restoring VM must have an open done channel")
	default:
	}
	require.ErrorIs(t, v.beginRestore(), ErrInvalidTransition)
	require.NoError(t, v.transition(StateStopped))
	<-v.Done()
}

func TestFirecrackerControlRefusedDuringSnapshot(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "kernel")
	v, err := m.store.Create(Config{Memory: "128M"})
	require.NoError(t, err)
	v.State = StateStopped
	close(v.done)
	require.NoError(t, m.claims.claim(v.ID, opSnapshotRestore))
	require.ErrorIs(t, m.Remove(context.Background(), v.ID), ErrVMBusy)
	require.ErrorIs(t, m.Stop(context.Background(), v.ID), ErrVMBusy)
	require.ErrorIs(t, m.Kill(context.Background(), v.ID), ErrVMBusy)
	require.ErrorIs(t, m.Signal(context.Background(), v.ID, syscall.SIGKILL), ErrVMBusy)
	require.ErrorIs(t, m.Signal(context.Background(), v.ID, syscall.SIGTERM), ErrVMBusy)
	require.ErrorIs(t, m.Start(context.Background(), v.ID), ErrVMBusy)
	m.claims.release(v.ID, opSnapshotRestore)
	require.NoError(t, m.Stop(context.Background(), v.ID), "stopped VM stop stays a no-op")
	require.NoError(t, m.Remove(context.Background(), v.ID))
}

type blockingRemoveStore struct {
	Store
	entered chan struct{}
	proceed chan struct{}
}

func (s *blockingRemoveStore) Remove(id string) error {
	close(s.entered)
	<-s.proceed
	return s.Store.Remove(id)
}

func TestRemoveOwnsLifecycleUntilDeletionFinishes(t *testing.T) {
	s := &blockingRemoveStore{Store: NewMemoryStore(), entered: make(chan struct{}), proceed: make(chan struct{})}
	m := NewFirecrackerManager("firecracker", "kernel", WithFCStore(s))
	v, err := s.Create(Config{})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- m.Remove(context.Background(), v.ID) }()
	<-s.entered
	_, release, claimErr := m.claimVM(v.ID, opSnapshotRestore)
	if release != nil {
		release()
	}
	close(s.proceed)
	require.NoError(t, <-done)
	require.ErrorIs(t, claimErr, ErrVMBusy)
}

type removedDuringResolveStore struct{ Store }

func (s *removedDuringResolveStore) Resolve(id string) (*VM, error) {
	v, err := s.Store.Resolve(id)
	if err == nil {
		_ = s.Remove(v.ID)
	}
	return v, err
}

func TestClaimRejectsVMRemovedAfterResolve(t *testing.T) {
	s := &removedDuringResolveStore{NewMemoryStore()}
	m := NewFirecrackerManager("firecracker", "kernel", WithFCStore(s))
	v, err := s.Create(Config{})
	require.NoError(t, err)
	_, _, err = m.claimVM(v.ID, opSnapshotRestore)
	require.ErrorContains(t, err, "removed or replaced")
	require.Empty(t, m.claims.current(v.ID))
}
