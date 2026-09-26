//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

func TestAutoRemoveWaitsForAttachResult(t *testing.T) {
	ctx := context.Background()
	mgr := vm.NewMockManager()
	s := &Server{mgr: mgr}
	v, err := mgr.Create(ctx, vm.Config{Attach: true})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx, v.ID))
	session := v.AttachSession()
	finished := make(chan struct{})
	go func() {
		s.autoRemoveSession(ctx, v, session)
		close(finished)
	}()
	require.NoError(t, mgr.Stop(ctx, v.ID))
	_, err = mgr.Get(v.ID)
	require.NoError(t, err, "a fast guest must survive until Attach receives its result")
	select {
	case <-finished:
		t.Fatal("auto-remove raced the attached result")
	default:
	}
	session.Finish()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("auto-remove did not finish after Attach")
	}
	_, err = mgr.Get(v.ID)
	require.Error(t, err)
}

func TestRemoveVMLease(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "manual"
		if automatic {
			name = "automatic"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, err := network.NewStore(t.TempDir())
			require.NoError(t, err)
			_, err = store.Create("bench", "10.100.0.0/29", "bridge")
			require.NoError(t, err)
			mgr := vm.NewMockManager()
			s := &Server{mgr: mgr, netStore: store}
			for range 10 {
				ip, err := store.AllocateIP("bench")
				require.NoError(t, err)
				v, err := mgr.Create(ctx, vm.Config{NetworkName: "bench", IPAddress: ip.String()})
				require.NoError(t, err)
				require.NoError(t, mgr.Start(ctx, v.ID))
				require.Error(t, s.removeVM(ctx, v.ID))
				require.ErrorIs(t, store.ReserveIP("bench", ip.String()), network.ErrIPAlreadyAllocated)
				require.NoError(t, mgr.Stop(ctx, v.ID))
				if automatic {
					s.autoRemove(ctx, v)
				} else {
					require.NoError(t, s.removeVM(ctx, v.ID))
				}
				require.NoError(t, store.ReserveIP("bench", ip.String()))
				require.NoError(t, store.ReleaseIP("bench", ip.String()))
			}
		})
	}
}

func TestRemoveVMFailureRetainsLease(t *testing.T) {
	store, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	_, err = store.Create("bench", "10.100.0.0/24", "bridge")
	require.NoError(t, err)
	ip, err := store.AllocateIP("bench")
	require.NoError(t, err)
	mgr := vm.NewMockManager()
	v, err := mgr.Create(context.Background(), vm.Config{NetworkName: "bench", IPAddress: ip.String()})
	require.NoError(t, err)
	mgr.RemoveFn = func(context.Context, string) error { return errors.New("disk failure") }
	s := &Server{mgr: mgr, netStore: store}
	require.ErrorContains(t, s.removeVM(context.Background(), v.ID), "disk failure")
	require.ErrorIs(t, store.ReserveIP("bench", ip.String()), network.ErrIPAlreadyAllocated)
}

func TestRemoveVMReleasesStaticIP(t *testing.T) {
	ctx := context.Background()
	store, err := network.NewStore(t.TempDir())
	require.NoError(t, err)
	_, err = store.Create("bench", "10.100.0.0/24", "bridge")
	require.NoError(t, err)
	mgr := vm.NewMockManager()
	s := &Server{mgr: mgr, netStore: store}
	const ip = "10.100.0.200"
	for range 3 {
		require.NoError(t, store.ReserveIP("bench", ip))
		v, err := mgr.Create(ctx, vm.Config{NetworkName: "bench", IPAddress: ip})
		require.NoError(t, err)
		require.NoError(t, mgr.Start(ctx, v.ID))
		require.NoError(t, mgr.Stop(ctx, v.ID))
		require.NoError(t, s.removeVM(ctx, v.ID))
	}
	require.NoError(t, store.ReserveIP("bench", ip))
}
