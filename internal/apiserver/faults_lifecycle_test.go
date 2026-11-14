//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hypervisor refuses to register the VM: the address the daemon picked
// for it goes back to the pool and the name stays free.
func TestRunFault_CreateFails(t *testing.T) {
	for _, static := range []string{"", "10.100.0.77"} {
		t.Run("ip="+static, func(t *testing.T) {
			e := newFaultEnv(t)
			e.mgr.CreateFn = func(context.Context, vm.Config) (*vm.VM, error) { return nil, errInjected }
			_, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet, IPAddress: static})
			requireRPCError(t, rpcErr, -32000, errInjected.Error())
			require.Empty(t, e.mgr.List())
			e.requireNoLeak()

			e.mgr.CreateFn = nil
			info, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet, IPAddress: static})
			require.Nil(t, rpcErr, "the name and the address must be usable again")
			require.Equal(t, "web", info.Name)
			e.requireNoLeak()
		})
	}
}

// The VM registers but does not boot: it must not stay behind as a VM nobody
// asked to keep, holding its name and its address.
func TestRunFault_StartFails(t *testing.T) {
	for _, static := range []string{"", "10.100.0.77"} {
		t.Run("ip="+static, func(t *testing.T) {
			e := newFaultEnv(t)
			e.failStart()
			_, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet, IPAddress: static})
			requireRPCError(t, rpcErr, -32000, errInjected.Error())
			require.Empty(t, e.mgr.List(), "a VM that failed to start stayed registered")
			e.requireNoLeak()

			e.mgr.StartFn = nil
			_, rpcErr = e.run(api.RunParams{Name: "web", NetworkName: faultNet, IPAddress: static})
			require.Nil(t, rpcErr, "the name and the address must be usable again")
			e.requireNoLeak()
		})
	}
}

// The VM does not boot and cannot be removed either. It is still registered,
// so its address must stay leased: releasing it would hand the address of a
// registered VM to the next one.
func TestRunFault_StartFailsAndCleanupFails(t *testing.T) {
	e := newFaultEnv(t)
	e.mgr.StartFn = func(context.Context, string) error { return errInjected }
	e.mgr.RemoveFn = func(context.Context, string) error { return fmt.Errorf("state directory busy") }
	_, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet})
	requireRPCError(t, rpcErr, -32000, errInjected.Error())
	require.Contains(t, rpcErr.Message, "state directory busy", "the user must learn that a VM was left behind")
	left := e.mgr.List()
	require.Len(t, left, 1)
	require.Contains(t, rpcErr.Message, left[0].ID, "the error must name the VM to clean up")
	e.requireNoLeak()
}

// Requests refused before anything is reserved leave nothing reserved.
func TestRunFault_RefusedBeforeCreate(t *testing.T) {
	e := newFaultEnv(t)
	cases := []struct {
		name string
		p    api.RunParams
		code int
		msg  string
	}{
		{"unknown network", api.RunParams{NetworkName: "nope"}, -32000, "not found"},
		{"hostile network name", api.RunParams{NetworkName: "../../etc"}, -32000, ""},
		{"address outside the subnet", api.RunParams{NetworkName: faultNet, IPAddress: "192.168.1.5"}, -32000, "outside"},
		{"address is the gateway", api.RunParams{NetworkName: faultNet, IPAddress: faultGateway}, -32000, "already allocated"},
		{"address is the network address", api.RunParams{NetworkName: faultNet, IPAddress: "10.100.0.0"}, -32000, "network or broadcast"},
		{"address is the broadcast address", api.RunParams{NetworkName: faultNet, IPAddress: "10.100.0.255"}, -32000, "network or broadcast"},
		{"address is not an address", api.RunParams{NetworkName: faultNet, IPAddress: "10.100.0.5; reboot"}, -32000, "not a valid IPv4"},
		{"address is IPv6", api.RunParams{NetworkName: faultNet, IPAddress: "fe80::1"}, -32000, "not a valid IPv4"},
		{"unknown image", api.RunParams{Image: "ghost:latest"}, -32000, "ghost:latest"},
		{"unknown volume", api.RunParams{NetworkName: faultNet, Volumes: []api.VolumeMountSpec{{Name: "ghost", GuestPath: "/data"}}}, api.CodeNotFound, "ghost"},
		{"volume by host path", api.RunParams{NetworkName: faultNet, Volumes: []api.VolumeMountSpec{{DiskPath: "/etc/passwd", GuestPath: "/data"}}}, -32602, "by name"},
		{"hostile volume name", api.RunParams{NetworkName: faultNet, Volumes: []api.VolumeMountSpec{{Name: "../data", GuestPath: "/data"}}}, -32000, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rpcErr := e.run(tc.p)
			requireRPCError(t, rpcErr, tc.code, tc.msg)
			require.Empty(t, e.mgr.List(), "a refused run registered a VM")
			e.requireNoLeak()
		})
	}
}

// A read-only volume that holds no filesystem is refused before a VM or an
// address exists, and the volume is not formatted behind the user's back.
func TestRunFault_UnusableVolume(t *testing.T) {
	e := newFaultEnv(t)
	vol, err := e.vols.Create("data", 1<<20)
	require.NoError(t, err)
	before, err := os.ReadFile(vol.DiskPath)
	require.NoError(t, err)

	_, rpcErr := e.run(api.RunParams{NetworkName: faultNet, Volumes: []api.VolumeMountSpec{{Name: "data", GuestPath: "/data", ReadOnly: true}}})
	requireRPCError(t, rpcErr, -32000, "seed it first")

	// A writable one needs the formatter, and the toolchain cannot be had.
	e.s.EnableVolumeFormatResolver(func(context.Context) (volume.Formatter, error) {
		return nil, fmt.Errorf("download toolchain: no route to host")
	})
	_, rpcErr = e.run(api.RunParams{NetworkName: faultNet, Volumes: []api.VolumeMountSpec{{Name: "data", GuestPath: "/data"}}})
	requireRPCError(t, rpcErr, -32000, "no route to host")

	// The formatter itself fails.
	e.s.EnableVolumeFormatResolver(func(context.Context) (volume.Formatter, error) {
		return func(context.Context, string, string, int64) error { return fmt.Errorf("mkfs exited 1") }, nil
	})
	_, rpcErr = e.run(api.RunParams{NetworkName: faultNet, Volumes: []api.VolumeMountSpec{{Name: "data", GuestPath: "/data"}}})
	requireRPCError(t, rpcErr, -32000, "mkfs exited 1")

	// The disk went missing under the store.
	require.NoError(t, os.Rename(vol.DiskPath, vol.DiskPath+".gone"))
	_, rpcErr = e.run(api.RunParams{NetworkName: faultNet, Volumes: []api.VolumeMountSpec{{Name: "data", GuestPath: "/data"}}})
	require.NotNil(t, rpcErr)
	require.NoError(t, os.Rename(vol.DiskPath+".gone", vol.DiskPath))

	after, err := os.ReadFile(vol.DiskPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused run changed the volume")
	require.Empty(t, e.mgr.List())
	e.requireNoLeak()
}

// Every address of the network is taken: the run is refused, and addresses
// come back as their VMs are removed.
func TestRunFault_SubnetExhausted(t *testing.T) {
	e := newFaultEnv(t)
	_, err := e.nets.Create("tiny", "10.200.0.0/29", "bridge")
	require.NoError(t, err)
	var ids []string
	var rpcErr *api.RPCError
	for range 16 {
		var info api.VMInfo
		if info, rpcErr = e.run(api.RunParams{NetworkName: "tiny"}); rpcErr != nil {
			break
		}
		ids = append(ids, info.ID)
	}
	requireRPCError(t, rpcErr, -32000, "no available IPs")
	require.NotEmpty(t, ids)
	require.Len(t, e.mgr.List(), len(ids), "the refused run registered a VM")
	seen := map[string]bool{}
	for _, v := range e.mgr.List() {
		require.False(t, seen[v.Cfg.IPAddress], "address %s handed out twice", v.Cfg.IPAddress)
		seen[v.Cfg.IPAddress] = true
	}

	require.NoError(t, e.mgr.Stop(context.Background(), ids[0]))
	_, rpcErr = e.call("VM.Remove", api.IDParams{ID: ids[0]})
	require.Nil(t, rpcErr)
	_, rpcErr = e.run(api.RunParams{NetworkName: "tiny"})
	require.Nil(t, rpcErr, "the address of a removed VM must be usable again")
}

// The lease file cannot be written (disk full, I/O error) at any step of any
// write a run makes. The run either fails with nothing registered and nothing
// leased, or completes; the network stays usable afterwards.
func TestRunFault_LeaseWriteFails(t *testing.T) {
	writes := func() int {
		e := newFaultEnv(t)
		return durabletest.Count(t, isStateFile, func() {
			_, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet})
			require.Nil(t, rpcErr)
		})
	}()
	require.Positive(t, writes, "a run on a network must persist its lease")

	for nth := range writes {
		for _, f := range durabletest.Failures() {
			if f.Crash() {
				continue // a crash kills the daemon; what a restart finds is tested with Serve
			}
			t.Run(fmt.Sprintf("write%d/%s", nth, f.Name), func(t *testing.T) {
				e := newFaultEnv(t)
				inj := durabletest.Inject(t, f, nth, isStateFile)
				_, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet})
				if writeFailed(inj, f) {
					requireRPCError(t, rpcErr, -32000, "allocate ip")
					require.Empty(t, e.mgr.List(), "a run that failed registered a VM")
				} else {
					require.Nil(t, rpcErr, "a step that does not fail the write must not fail the run")
				}
				e.requireNoLeak()

				// The fault is gone: the same request now goes through.
				if rpcErr != nil {
					_, rpcErr = e.run(api.RunParams{Name: "web", NetworkName: faultNet})
					require.Nil(t, rpcErr)
				}
				_, rpcErr = e.run(api.RunParams{Name: "db", NetworkName: faultNet})
				require.Nil(t, rpcErr)
				e.requireNoLeak()
				require.Len(t, e.leases(), 2)
			})
		}
	}
}

// A static address is reserved with a write of its own; the same holds.
func TestRunFault_StaticLeaseWriteFails(t *testing.T) {
	for _, f := range durabletest.Failures() {
		if f.Crash() {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			e := newFaultEnv(t)
			inj := durabletest.Inject(t, f, 0, isStateFile)
			_, rpcErr := e.run(api.RunParams{NetworkName: faultNet, IPAddress: "10.100.0.9"})
			if writeFailed(inj, f) {
				requireRPCError(t, rpcErr, -32000, "reserve ip")
				require.Empty(t, e.mgr.List())
				e.requireNoLeak()
				_, rpcErr = e.run(api.RunParams{NetworkName: faultNet, IPAddress: "10.100.0.9"})
				require.Nil(t, rpcErr, "the address must still be free after a reservation that failed")
			}
			e.requireNoLeak()
		})
	}
}

// The VM does not boot and, on top of that, its address cannot be given back
// because the lease file cannot be written. The VM must still be gone and the
// error must say that the address was not released.
func TestRunFault_StartFailsAndReleaseWriteFails(t *testing.T) {
	e := newFaultEnv(t)
	e.failStart()
	inj := durabletest.Inject(t, durabletest.Failures()[0], 1, isStateFile)
	_, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet})
	require.True(t, inj.Fired())
	requireRPCError(t, rpcErr, -32000, errInjected.Error())
	require.Contains(t, rpcErr.Message, "release IP")
	require.Empty(t, e.mgr.List())

	// The address is still recorded; the documented way out works.
	held := e.leases()
	require.Len(t, held, 1)
	_, rpcErr = e.call("Network.ReleaseIP", map[string]string{"network": faultNet, "ip": held[0]})
	require.Nil(t, rpcErr)
	e.requireNoLeak()
}

// VM.Start of a stopped VM boots a replacement. Whatever step fails, the
// stopped VM is still there to be started again, with its address, and the
// start is not left claimed.
func TestStartFault_ReplacementFails(t *testing.T) {
	t.Run("create fails", func(t *testing.T) {
		e := newFaultEnv(t)
		old := e.stoppedVM("web")
		e.mgr.CreateFn = func(context.Context, vm.Config) (*vm.VM, error) { return nil, errInjected }
		_, rpcErr := e.call("VM.Start", api.IDParams{ID: old.ID})
		requireRPCError(t, rpcErr, -32000, errInjected.Error())
		require.Len(t, e.mgr.List(), 1)
		require.Equal(t, vm.StateStopped, old.GetState())
		e.requireNoLeak()

		e.mgr.CreateFn = nil
		res, rpcErr := e.call("VM.Start", api.IDParams{ID: old.ID})
		require.Nil(t, rpcErr, "the VM must still be startable")
		info, ok := res.(api.VMInfo)
		require.True(t, ok)
		require.NotEqual(t, old.ID, info.ID)
		require.Equal(t, "running", info.State)
		require.Len(t, e.mgr.List(), 1, "the stopped predecessor must be gone")
		require.Equal(t, old.Cfg.IPAddress, e.mgr.List()[0].Cfg.IPAddress, "a restarted VM keeps its address")
		e.requireNoLeak()
	})

	t.Run("boot fails", func(t *testing.T) {
		e := newFaultEnv(t)
		old := e.stoppedVM("web")
		e.failStart()
		_, rpcErr := e.call("VM.Start", api.IDParams{ID: old.ID})
		requireRPCError(t, rpcErr, -32000, errInjected.Error())
		left := e.mgr.List()
		require.Len(t, left, 1, "the unbootable replacement stayed registered")
		require.Equal(t, old.ID, left[0].ID)
		e.requireNoLeak()

		e.mgr.StartFn = nil
		_, rpcErr = e.call("VM.Start", api.IDParams{ID: old.ID})
		require.Nil(t, rpcErr)
		e.requireNoLeak()
	})

	t.Run("predecessor cannot be removed", func(t *testing.T) {
		e := newFaultEnv(t)
		old := e.stoppedVM("web")
		e.mgr.RemoveFn = func(context.Context, string) error { return errInjected }
		res, rpcErr := e.call("VM.Start", api.IDParams{ID: old.ID})
		require.Nil(t, rpcErr, "the replacement is running: the start succeeded")
		info, ok := res.(api.VMInfo)
		require.True(t, ok)
		require.Equal(t, "running", info.State)
		require.Equal(t, []string{old.Cfg.IPAddress}, e.leases(), "the address stays leased, once")
	})
}

// A VM that was registered but never booted is started in place.
func TestStartFault_CreatedVM(t *testing.T) {
	e := newFaultEnv(t)
	v, err := e.mgr.Create(context.Background(), vm.Config{Name: "web"})
	require.NoError(t, err)
	e.mgr.StartFn = func(context.Context, string) error { return errInjected }
	_, rpcErr := e.call("VM.Start", api.IDParams{ID: v.ID})
	requireRPCError(t, rpcErr, -32000, errInjected.Error())
	require.Len(t, e.mgr.List(), 1)
	e.requireNoLeak()

	e.mgr.StartFn = nil
	res, rpcErr := e.call("VM.Start", api.IDParams{ID: v.ID})
	require.Nil(t, rpcErr)
	info, ok := res.(api.VMInfo)
	require.True(t, ok)
	require.Equal(t, v.ID, info.ID, "a created VM is started in place, not replaced")

	// It runs now, and a running VM is not started twice.
	_, rpcErr = e.call("VM.Start", api.IDParams{ID: v.ID})
	requireRPCError(t, rpcErr, -32000, "only stopped VMs can be started")
	require.Len(t, e.mgr.List(), 1)
	e.requireNoLeak()
}

// The VM disappears between the lookup and the claim of its start.
func TestStartFault_VMVanishesUnderTheClaim(t *testing.T) {
	e := newFaultEnv(t)
	old := e.stoppedVM("web")
	calls := 0
	e.mgr.GetFn = func(id string) (*vm.VM, error) {
		calls++
		if calls > 1 {
			return nil, fmt.Errorf("vm %s not found", id)
		}
		return old, nil
	}
	_, rpcErr := e.call("VM.Start", api.IDParams{ID: old.ID})
	requireRPCError(t, rpcErr, -32000, "not found")
	e.mgr.GetFn = nil
	require.Len(t, e.mgr.List(), 1, "no replacement may be created for a VM that is gone")
	e.requireNoLeak()
}

// Removing a VM writes the lease file twice: the intent, then the release.
// A failed intent keeps the VM and its lease. A failed release happens after
// the VM is gone, so the error must say so, and the next daemon start must
// finish the release from the recorded intent.
func TestRemoveFault_LeaseWriteFails(t *testing.T) {
	for _, f := range durabletest.Failures() {
		if f.Crash() || f.Step == durable.StepSync || f.Step == durable.StepSyncDir {
			continue // the lease file is written without these steps
		}
		t.Run("intent/"+f.Name, func(t *testing.T) {
			e := newFaultEnv(t)
			v := e.stoppedVM("web")
			durabletest.Inject(t, f, 0, isStateFile)
			_, rpcErr := e.call("VM.Remove", api.IDParams{ID: v.ID})
			requireRPCError(t, rpcErr, -32000, "")
			require.Len(t, e.mgr.List(), 1, "the VM must survive a removal that could not be recorded")
			e.requireNoLeak()

			_, rpcErr = e.call("VM.Remove", api.IDParams{ID: v.ID})
			require.Nil(t, rpcErr)
			require.Empty(t, e.mgr.List())
			e.requireNoLeak()
		})
		t.Run("release/"+f.Name, func(t *testing.T) {
			e := newFaultEnv(t)
			v := e.stoppedVM("web")
			ip := v.Cfg.IPAddress
			durabletest.Inject(t, f, 1, isStateFile)
			_, rpcErr := e.call("VM.Remove", api.IDParams{ID: v.ID})
			requireRPCError(t, rpcErr, -32000, "VM removed, but release of "+ip)
			require.Empty(t, e.mgr.List())
			require.ErrorIs(t, e.nets.ReserveIP(faultNet, ip), network.ErrIPAlreadyAllocated, "the address is still recorded as leased")

			// The daemon restarts: the recorded intent frees the address.
			e.restart()
			require.Empty(t, e.leases(), "the release must be finished at the next start")
			require.NoError(t, e.nets.ReserveIP(faultNet, ip))
		})
	}
}

// The hypervisor refuses to remove the VM after the intent was recorded. The
// VM is still registered, so a daemon restart must not free its address.
func TestRemoveFault_ManagerFailsThenDaemonRestarts(t *testing.T) {
	e := newFaultEnv(t)
	v := e.stoppedVM("web")
	e.mgr.RemoveFn = func(context.Context, string) error { return errInjected }
	_, rpcErr := e.call("VM.Remove", api.IDParams{ID: v.ID})
	requireRPCError(t, rpcErr, -32000, errInjected.Error())
	e.mgr.RemoveFn = nil
	require.Len(t, e.mgr.List(), 1)

	e.restart()
	require.Equal(t, []string{v.Cfg.IPAddress}, e.leases(), "a registered VM lost its address at restart")
	e.requireNoLeak()
}

// Two VMs were left sharing one address (a failed predecessor removal after
// a restart). Removing one must not free the address the other still uses.
func TestRemoveFault_SharedAddressIsKept(t *testing.T) {
	e := newFaultEnv(t)
	old := e.stoppedVM("web")
	e.mgr.RemoveFn = func(context.Context, string) error { return errInjected }
	res, rpcErr := e.call("VM.Start", api.IDParams{ID: old.ID})
	require.Nil(t, rpcErr)
	e.mgr.RemoveFn = nil
	require.Len(t, e.mgr.List(), 2)

	_, rpcErr = e.call("VM.Remove", api.IDParams{ID: old.ID})
	require.Nil(t, rpcErr)
	info, ok := res.(api.VMInfo)
	require.True(t, ok)
	require.Equal(t, []string{old.Cfg.IPAddress}, e.leases(), "the running replacement lost its address")
	require.Len(t, e.mgr.List(), 1)
	assert.Equal(t, info.ID, e.mgr.List()[0].ID)
}

// The network of a VM was removed while the VM was stopped. The VM must still
// be removable; there is no lease left to release.
func TestRemoveFault_NetworkIsGone(t *testing.T) {
	e := newFaultEnv(t)
	v := e.stoppedVM("web")
	require.NoError(t, e.nets.Remove(faultNet))
	_, rpcErr := e.call("VM.Remove", api.IDParams{ID: v.ID})
	require.Nil(t, rpcErr, "a VM whose network is gone must still be removable")
	require.Empty(t, e.mgr.List())
}

// Stop, kill and signal failures are reported and change nothing.
func TestStopKillSignalFault_ManagerFails(t *testing.T) {
	e := newFaultEnv(t)
	info, rpcErr := e.run(api.RunParams{Name: "web", NetworkName: faultNet})
	require.Nil(t, rpcErr)
	e.mgr.StopFn = func(context.Context, string) error { return errInjected }
	e.mgr.KillFn = func(context.Context, string) error { return errInjected }
	e.mgr.SignalFn = func(context.Context, string, os.Signal) error { return errInjected }

	for _, tc := range []struct {
		method string
		params any
	}{
		{"VM.Stop", api.StopParams{ID: info.ID}},
		{"VM.Stop", api.StopParams{ID: info.ID, Force: true}},
		{"VM.Kill", api.IDParams{ID: info.ID}},
		{"VM.Signal", api.SignalParams{ID: info.ID, Signal: "SIGTERM"}},
	} {
		_, rpcErr := e.call(tc.method, tc.params)
		requireRPCError(t, rpcErr, -32000, errInjected.Error())
	}
	v, err := e.mgr.Get(info.ID)
	require.NoError(t, err)
	require.Equal(t, vm.StateRunning, v.GetState())
	e.requireNoLeak()

	// A running VM is not removed, and keeps its address.
	_, rpcErr = e.call("VM.Remove", api.IDParams{ID: info.ID})
	requireRPCError(t, rpcErr, -32000, "must be stopped")
	e.requireNoLeak()
}

// An auto-removed VM whose removal fails stays registered with its lease
// rather than being half-forgotten.
func TestAutoRemoveFault_RemovalFails(t *testing.T) {
	e := newFaultEnv(t)
	info, rpcErr := e.run(api.RunParams{Name: "job", NetworkName: faultNet})
	require.Nil(t, rpcErr)
	v, err := e.mgr.Get(info.ID)
	require.NoError(t, err)
	e.mgr.RemoveFn = func(context.Context, string) error { return errInjected }
	require.NoError(t, e.mgr.Stop(context.Background(), v.ID))
	e.s.autoRemove(context.Background(), v)
	e.mgr.RemoveFn = nil
	require.Len(t, e.mgr.List(), 1)
	e.requireNoLeak()
}
