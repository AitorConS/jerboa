//go:build linux

package vm

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeQEMUCmd returns a CommandFunc that spawns a fake QEMU process.
// If block is true the process runs until killed; otherwise it exits
// immediately. Cross-platform: uses sleep/true on Unix and PowerShell/cmd
// on Windows.
func fakeQEMUCmd(block bool) CommandFunc {
	return func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		if block {
			if runtime.GOOS == "windows" {
				return exec.Command("powershell", "-Command", "while ($true) { Start-Sleep -Seconds 3600 }")
			}
			return exec.Command("sleep", "3600")
		}
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/c", "exit 0")
		}
		return exec.Command("true")
	}
}

func fakeManager(block bool) *QEMUManager {
	return NewQEMUManager("fake-qemu", WithCommandFunc(fakeQEMUCmd(block)))
}

// --- state machine tests ---

func TestVM_transition_valid(t *testing.T) {
	cases := []struct {
		name string
		from State
		to   State
	}{
		{"created→starting", StateCreated, StateStarting},
		{"starting→running", StateStarting, StateRunning},
		{"starting→stopped", StateStarting, StateStopped},
		{"running→stopping", StateRunning, StateStopping},
		{"running→stopped", StateRunning, StateStopped},
		{"stopping→stopped", StateStopping, StateStopped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &VM{ID: "test", State: tc.from, done: make(chan struct{})}
			require.NoError(t, v.transition(tc.to))
			require.Equal(t, tc.to, v.GetState())
		})
	}
}

func TestVM_transition_invalid(t *testing.T) {
	cases := []struct {
		from State
		to   State
	}{
		{StateCreated, StateRunning},
		{StateCreated, StateStopped},
		{StateRunning, StateCreated},
		{StateStopped, StateRunning},
		{StateStopped, StateStarting},
	}
	for _, tc := range cases {
		v := &VM{ID: "test", State: tc.from, done: make(chan struct{})}
		require.Error(t, v.transition(tc.to))
		require.Equal(t, tc.from, v.GetState())
	}
}

func TestVM_done_closed_on_stopped(t *testing.T) {
	v := &VM{ID: "test", State: StateRunning, done: make(chan struct{})}
	require.NoError(t, v.transition(StateStopped))
	select {
	case <-v.Done():
	default:
		t.Fatal("done channel not closed after StateStopped")
	}
}

// --- Store tests ---

func TestStore_Create(t *testing.T) {
	s := NewStore()
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NotEmpty(t, v.ID)
	require.Equal(t, StateCreated, v.GetState())
}

func TestStore_Get(t *testing.T) {
	s := NewStore()
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	got, err := s.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
}

func TestStore_GetNotFound(t *testing.T) {
	s := NewStore()
	_, err := s.Get("nonexistent")
	require.Error(t, err)
}

func TestStore_List(t *testing.T) {
	s := NewStore()
	_, err := s.Create(Config{ImagePath: "a.img", Memory: "256M"})
	require.NoError(t, err)
	_, err = s.Create(Config{ImagePath: "b.img", Memory: "256M"})
	require.NoError(t, err)
	require.Len(t, s.List(), 2)
}

func TestStore_Remove(t *testing.T) {
	s := NewStore()
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, s.Remove(v.ID))
	require.Empty(t, s.List())
}

func TestStore_RemoveNotFound(t *testing.T) {
	s := NewStore()
	require.Error(t, s.Remove("nonexistent"))
}

// --- QEMUManager tests (with injected fake command) ---

func TestQEMUManager_Create(t *testing.T) {
	mgr := fakeManager(false)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NotEmpty(t, v.ID)
	require.Equal(t, StateCreated, v.GetState())
}

func TestQEMUManager_Remove_not_stopped(t *testing.T) {
	mgr := fakeManager(false)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.Error(t, mgr.Remove(context.Background(), v.ID))
}

func TestQEMUManager_Start_transitions_to_running(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.Equal(t, StateRunning, v.GetState())

	require.NoError(t, mgr.Stop(context.Background(), v.ID))
	select {
	case <-v.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VM did not stop")
	}
}

func TestQEMUManager_Stop_transitions_to_stopped(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.NoError(t, mgr.Stop(context.Background(), v.ID))

	select {
	case <-v.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VM did not stop after kill")
	}
	require.Equal(t, StateStopped, v.GetState())
}

func TestQEMUManager_process_exit_stops_vm(t *testing.T) {
	mgr := fakeManager(false) // exits immediately
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	require.NoError(t, mgr.Start(context.Background(), v.ID))

	select {
	case <-v.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VM did not reach StateStopped after process exit")
	}
	require.Equal(t, StateStopped, v.GetState())
}

func TestQEMUManager_Remove_after_stop(t *testing.T) {
	mgr := fakeManager(false)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	require.NoError(t, mgr.Start(context.Background(), v.ID))
	<-v.Done()

	require.NoError(t, mgr.Remove(context.Background(), v.ID))
	require.Empty(t, mgr.List())
}

func TestQEMUManager_List(t *testing.T) {
	mgr := fakeManager(true)
	_, err := mgr.Create(context.Background(), Config{ImagePath: "a.img", Memory: "256M"})
	require.NoError(t, err)
	_, err = mgr.Create(context.Background(), Config{ImagePath: "b.img", Memory: "256M"})
	require.NoError(t, err)
	require.Len(t, mgr.List(), 2)
}

// --- buildCmd / buildNetArgs / buildEnvArgs tests ---

func captureArgs(mgr *QEMUManager, cfg Config) []string {
	var got []string
	mgr.mkCmd = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		got = args
		return fakeQEMUCmd(false)(context.Background(), "", args...)
	}
	_ = mgr.buildCmd(context.Background(), cfg, "")
	return got
}

func TestBuildCmd_no_network(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{ImagePath: "disk.img", Memory: "256M"})
	require.Contains(t, args, "-net")
	idx := indexOf(args, "-net")
	require.Equal(t, "none", args[idx+1])
}

func TestBuildCmd_slirp_single_port(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
		PortMaps:  []PortMap{{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}},
	})
	// -net none must NOT appear
	require.NotContains(t, args, "none")
	// -netdev user,id=net0,hostfwd=tcp::8080-:80
	idx := indexOf(args, "-netdev")
	require.GreaterOrEqual(t, idx, 0)
	require.Contains(t, args[idx+1], "hostfwd=tcp::8080-:80")
}

func TestBuildCmd_slirp_multiple_ports(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
		PortMaps: []PortMap{
			{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP},
			{HostPort: 5353, GuestPort: 53, Protocol: ProtocolUDP},
		},
	})
	idx := indexOf(args, "-netdev")
	require.GreaterOrEqual(t, idx, 0)
	netdev := args[idx+1]
	require.Contains(t, netdev, "hostfwd=tcp::8080-:80")
	require.Contains(t, netdev, "hostfwd=udp::5353-:53")
}

func TestBuildCmd_tap_overrides_portmaps(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath:   "disk.img",
		Memory:      "256M",
		NetworkName: "uni-tap0",
		PortMaps:    []PortMap{{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}},
	})
	idx := indexOf(args, "-netdev")
	require.GreaterOrEqual(t, idx, 0)
	require.Contains(t, args[idx+1], "tap,id=net0,ifname=uni-tap0")
	require.NotContains(t, args[idx+1], "hostfwd")
}

func TestBuildCmd_env_vars(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
		Env:       []string{"FOO=bar", "PORT=8080"},
	})
	idx := indexOf(args, "-fw_cfg")
	require.GreaterOrEqual(t, idx, 0, "-fw_cfg flag must be present")
	fwcfg := args[idx+1]
	require.True(t, strings.HasPrefix(fwcfg, "name=opt/uni/env,string="))
	require.Contains(t, fwcfg, "FOO=bar")
	require.Contains(t, fwcfg, "PORT=8080")
}

func TestBuildCmd_no_env_no_fwcfg(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{ImagePath: "disk.img", Memory: "256M"})
	require.NotContains(t, args, "-fw_cfg")
}

func TestBuildCmd_cpus(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{ImagePath: "disk.img", Memory: "512M", CPUs: 4})
	idx := indexOf(args, "-smp")
	require.GreaterOrEqual(t, idx, 0)
	require.Equal(t, "4", args[idx+1])
}

func TestBuildCmd_network_cfg(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath:   "disk.img",
		Memory:      "256M",
		NetworkName: "uni-tap0",
		IPAddress:   "10.0.0.2",
		GatewayIP:   "10.0.0.1",
	})
	idx := indexOf(args, "-fw_cfg")
	require.GreaterOrEqual(t, idx, 0, "-fw_cfg flag must be present")
	count := 0
	for i, v := range args {
		if v == "-fw_cfg" && i+1 < len(args) && strings.HasPrefix(args[i+1], "name=opt/uni/network") {
			count++
			require.Contains(t, args[i+1], "10.0.0.2/24,10.0.0.1")
		}
	}
	require.Equal(t, 1, count)
}

func TestBuildCmd_no_network_cfg_without_ip(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
	})
	for i, v := range args {
		if v == "-fw_cfg" && i+1 < len(args) {
			require.NotContains(t, args[i+1], "opt/uni/network")
		}
	}
}

func TestBuildCmd_disk_throttle_iops(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
		DiskIOPS:  1000,
	})
	idx := indexOf(args, "-drive")
	require.GreaterOrEqual(t, idx, 0)
	require.Contains(t, args[idx+1], "throttling.iops-total=1000")
}

func TestBuildCmd_disk_throttle_bps(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
		DiskBPS:   10 * 1024 * 1024,
	})
	idx := indexOf(args, "-drive")
	require.GreaterOrEqual(t, idx, 0)
	require.Contains(t, args[idx+1], "throttling.bps-total=10485760")
}

func TestBuildCmd_disk_throttle_both(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
		DiskIOPS:  500,
		DiskBPS:   5 * 1024 * 1024,
	})
	idx := indexOf(args, "-drive")
	require.GreaterOrEqual(t, idx, 0)
	require.Contains(t, args[idx+1], "throttling.iops-total=500")
	require.Contains(t, args[idx+1], "throttling.bps-total=5242880")
}

func TestBuildCmd_no_throttle_when_zero(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu")
	args := captureArgs(mgr, Config{
		ImagePath: "disk.img",
		Memory:    "256M",
	})
	idx := indexOf(args, "-drive")
	require.GreaterOrEqual(t, idx, 0)
	require.NotContains(t, args[idx+1], "throttling")
}

func indexOf(slice []string, s string) int {
	for i, v := range slice {
		if v == s {
			return i
		}
	}
	return -1
}
