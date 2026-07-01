//go:build linux

package scheduler

import (
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
)

type fakeSource struct {
	vms []*vm.VM
}

func (f *fakeSource) List() []*vm.VM {
	return f.vms
}

func TestResolverResolve(t *testing.T) {
	vms := []*vm.VM{
		{ID: "vm-1", State: vm.StateRunning, Cfg: vm.Config{Name: "frontend", NetworkName: "app", IPAddress: "10.100.1.2"}, CreatedAt: time.Now()},
		{ID: "vm-2", State: vm.StateStopped, Cfg: vm.Config{Name: "db", NetworkName: "app", IPAddress: "10.100.1.3"}, CreatedAt: time.Now()},
	}
	r := NewResolver(&fakeSource{vms: vms})

	rec, err := r.Resolve("frontend", "app")
	require.NoError(t, err)
	require.Equal(t, "10.100.1.2", rec.IP)

	rec, err = r.Resolve("vm-1", "app")
	require.NoError(t, err)
	require.Equal(t, "frontend", rec.Name)

	rec, err = r.Resolve("frontend.app", "")
	require.NoError(t, err)
	require.Equal(t, "app", rec.Network)

	_, err = r.Resolve("db", "app")
	require.Error(t, err)

	_, err = r.Resolve("", "app")
	require.Error(t, err)
}

func TestResolverList(t *testing.T) {
	vms := []*vm.VM{
		{ID: "vm-1", State: vm.StateRunning, Cfg: vm.Config{Name: "frontend", NetworkName: "app", IPAddress: "10.100.1.2"}, CreatedAt: time.Now()},
		{ID: "vm-2", State: vm.StateRunning, Cfg: vm.Config{Name: "backend", NetworkName: "app", IPAddress: "10.100.1.3"}, CreatedAt: time.Now()},
		{ID: "vm-3", State: vm.StateRunning, Cfg: vm.Config{Name: "cache", NetworkName: "cache", IPAddress: "10.100.2.2"}, CreatedAt: time.Now()},
	}
	r := NewResolver(&fakeSource{vms: vms})

	recs := r.List("app")
	require.Len(t, recs, 2)

	recs = r.List("")
	require.Len(t, recs, 3)
}

func TestResolverNetworkForIP(t *testing.T) {
	vms := []*vm.VM{
		{ID: "vm-1", State: vm.StateRunning, Cfg: vm.Config{Name: "web", NetworkName: "app", IPAddress: "10.100.0.3"}, CreatedAt: time.Now()},
		{ID: "vm-2", State: vm.StateRunning, Cfg: vm.Config{Name: "cache", NetworkName: "other", IPAddress: "10.200.0.5"}, CreatedAt: time.Now()},
		{ID: "vm-3", State: vm.StateStopped, Cfg: vm.Config{Name: "old", NetworkName: "app", IPAddress: "10.100.0.9"}, CreatedAt: time.Now()},
	}
	r := NewResolver(&fakeSource{vms: vms})

	require.Equal(t, "app", r.NetworkForIP("10.100.0.3"))
	require.Equal(t, "other", r.NetworkForIP("10.200.0.5"))
	// A stopped VM is not a live record.
	require.Empty(t, r.NetworkForIP("10.100.0.9"))
	// Unknown / empty addresses resolve to no network.
	require.Empty(t, r.NetworkForIP("10.100.0.99"))
	require.Empty(t, r.NetworkForIP(""))
}

func TestResolverResolveAmbiguous(t *testing.T) {
	vms := []*vm.VM{
		{ID: "vm-1", State: vm.StateRunning, Cfg: vm.Config{Name: "api", NetworkName: "app-a", IPAddress: "10.100.1.2"}, CreatedAt: time.Now()},
		{ID: "vm-2", State: vm.StateRunning, Cfg: vm.Config{Name: "api", NetworkName: "app-b", IPAddress: "10.100.2.2"}, CreatedAt: time.Now()},
	}
	r := NewResolver(&fakeSource{vms: vms})

	_, err := r.Resolve("api", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "ambiguous")

	rec, err := r.Resolve("api", "app-b")
	require.NoError(t, err)
	require.Equal(t, "10.100.2.2", rec.IP)
}
