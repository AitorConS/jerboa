//go:build linux || (darwin && arm64)

package scheduler

import (
	"fmt"
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

func TestResolverDottedAliasPriority(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(fmt.Sprint("collision=", collision), func(t *testing.T) {
			source := &fakeSource{vms: []*vm.VM{
				{ID: "pg", State: vm.StateRunning, Cfg: vm.Config{Name: "postgres", NetworkName: "app", IPAddress: "172.25.0.2", NetworkAliases: []string{"database.app", "db.internal"}}},
				{ID: "other", State: vm.StateRunning, Cfg: vm.Config{Name: "postgres", NetworkName: "other", IPAddress: "172.26.0.2", NetworkAliases: []string{"database.app", "other-only.app"}}},
			}}
			if collision {
				source.vms = append(source.vms, &vm.VM{ID: "db", State: vm.StateRunning, Cfg: vm.Config{Name: "database", NetworkName: "app", IPAddress: "172.25.0.3"}})
			}
			r := NewResolver(source)
			for _, q := range []struct{ name, network, id string }{
				{"database.app", "app", "pg"}, {"database.app", "", "pg"},
				{"database.app.app", "app", "pg"}, {"database.app.app", "", "pg"},
				{"postgres.app", "app", "pg"}, {"postgres.app", "", "pg"},
				{"db.internal", "app", "pg"}, {"db.internal.app", "app", "pg"},
				{"database.app", "other", "other"},
			} {
				rec, err := r.Resolve(q.name, q.network)
				require.NoError(t, err, "%+v", q)
				require.Equal(t, q.id, rec.VMID)
				all, err := r.ResolveAll(q.name, q.network)
				require.NoError(t, err)
				require.Equal(t, []Record{rec}, all)
			}
			for _, q := range []struct{ name, network string }{
				{"postgres.other", "app"}, {"other-only.app", "app"},
				{"database.app", "absent"}, {"db.internal", "other"},
			} {
				_, err := r.Resolve(q.name, q.network)
				require.Error(t, err)
				_, err = r.ResolveAll(q.name, q.network)
				require.Error(t, err)
			}
			// Multiple exact aliases must stay a replica set, never fall back to database.
			source.vms = append(source.vms, &vm.VM{ID: "pg2", State: vm.StateRunning, Cfg: vm.Config{Name: "postgres2", NetworkName: "app", IPAddress: "172.25.0.4", NetworkAliases: []string{"database.app"}}})
			all, err := r.ResolveAll("database.app", "app")
			require.NoError(t, err)
			require.Len(t, all, 2)
			require.Equal(t, "pg", all[0].VMID)
			require.Equal(t, "pg2", all[1].VMID)
			_, err = r.Resolve("database.app", "app")
			require.Error(t, err)
		})
	}
}
