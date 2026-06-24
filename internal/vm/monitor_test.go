//go:build linux

package vm

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQEMUManager_Start_AttachMode(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M", Attach: true})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.Equal(t, StateRunning, v.GetState())
	v.mu.RLock()
	reader := v.logPipeReader
	writer := v.logPipeWriter
	v.mu.RUnlock()
	require.NotNil(t, reader)
	require.NotNil(t, writer)
	require.NoError(t, mgr.Stop(context.Background(), v.ID))
	<-v.Done()
}

func TestQEMUManager_Start_StoreSave(t *testing.T) {
	store := NewMemoryStore()
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeQEMUCmd(true)), WithStore(store))
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.Equal(t, StateRunning, v.GetState())
	require.NoError(t, mgr.Stop(context.Background(), v.ID))
	<-v.Done()
}

func TestQEMUManager_Kill_ProcessKillFails(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	v.mu.Lock()
	v.proc = &mockProcess{killErr: os.ErrProcessDone}
	v.mu.Unlock()
	require.NoError(t, mgr.Kill(context.Background(), v.ID))
}

func TestQEMUManager_Kill_ProcessKillRealError(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	v.mu.Lock()
	v.proc = &mockProcess{killErr: syscall.ENOENT}
	v.mu.Unlock()
	err = mgr.Kill(context.Background(), v.ID)
	require.Error(t, err)
}

func TestQEMUManager_Stop_SIGTERMFails(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	v.mu.Lock()
	v.proc = &mockProcess{signalErr: syscall.ENOENT}
	v.mu.Unlock()
	err = mgr.Stop(context.Background(), v.ID)
	_ = err
}

func TestMonitor_ExplicitStop_NoRestart(t *testing.T) {
	mgr := fakeManager(false)
	ctx := context.Background()
	v, err := mgr.Create(context.Background(), Config{
		ImagePath: "test.img",
		Memory:    "256M",
		Restart:   RestartConfig{Policy: RestartAlways},
	})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx, v.ID))
	<-v.Done()
	require.Equal(t, StateStopped, v.GetState())
}

func TestMonitor_RestartAlways(t *testing.T) {
	var callCount int
	cmdFunc := func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		callCount++
		if callCount <= 1 {
			return exec.Command("false")
		}
		return exec.Command("sleep", "3600")
	}
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(cmdFunc))
	v, err := mgr.Create(context.Background(), Config{
		ImagePath: "test.img",
		Memory:    "256M",
		Restart:   RestartConfig{Policy: RestartAlways, MaxRetries: 3},
	})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.Eventually(t, func() bool {
		for _, vm := range mgr.List() {
			if vm.GetRestartCount() >= 1 && vm.GetState() == StateRunning {
				return true
			}
		}
		return false
	}, 15*time.Second, 200*time.Millisecond)
	var newV *VM
	for _, vm := range mgr.List() {
		if vm.GetRestartCount() >= 1 {
			newV = vm
			break
		}
	}
	require.NotNil(t, newV)
	require.NoError(t, mgr.Stop(context.Background(), newV.ID))
	select {
	case <-newV.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("replacement VM did not stop")
	}
}

func TestMonitor_RestartOnFailure_CrashExit(t *testing.T) {
	crashCount := 0
	cmdFunc := func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		crashCount++
		if crashCount <= 1 {
			return exec.Command("false")
		}
		return exec.Command("true")
	}
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(cmdFunc))
	v, err := mgr.Create(context.Background(), Config{
		ImagePath: "test.img",
		Memory:    "256M",
		Restart:   RestartConfig{Policy: RestartOnFailure, MaxRetries: 1},
	})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.Eventually(t, func() bool {
		for _, vm := range mgr.List() {
			if vm.GetRestartCount() >= 1 {
				return true
			}
		}
		return false
	}, 10*time.Second, 200*time.Millisecond)
}

func TestMonitor_RestartMaxRetries(t *testing.T) {
	cmdFunc := func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.Command("false")
	}
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(cmdFunc))
	v, err := mgr.Create(context.Background(), Config{
		ImagePath: "test.img",
		Memory:    "256M",
		Restart:   RestartConfig{Policy: RestartAlways, MaxRetries: 2},
	})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.Eventually(t, func() bool {
		for _, vm := range mgr.List() {
			if vm.GetState() == StateStopped && vm.GetRestartCount() >= 2 {
				return true
			}
		}
		return false
	}, 30*time.Second, 500*time.Millisecond)
}

func TestQEMUManager_Stop_ContextCancel(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = mgr.Stop(ctx, v.ID)
	select {
	case <-v.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("VM did not stop")
	}
}

func TestQEMUManager_Start_LogCapture(t *testing.T) {
	mgr := fakeManager(false)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	<-v.Done()
}

func TestQEMUManager_Start_FailsLaunch(t *testing.T) {
	mgr := NewQEMUManager("nonexistent-qemu-binary-xyz")
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	err = mgr.Start(context.Background(), v.ID)
	require.Error(t, err)
	require.Equal(t, StateStopped, v.GetState())
}

func TestHealthChecker_Run_DoneStops(t *testing.T) {
	hc := NewHealthChecker()
	done := make(chan struct{})
	p := &healthProbe{
		vm: &VM{
			ID:    "done-stop",
			Cfg:   Config{PortMaps: []PortMap{{HostPort: 1, GuestPort: 80, Protocol: ProtocolTCP}}},
			State: StateRunning,
			done:  make(chan struct{}),
		},
		cfg:  HealthCheckConfig{Type: "tcp", Port: 1, Interval: 50 * time.Millisecond, Timeout: 50 * time.Millisecond, Retries: 1},
		done: done,
	}
	hc.mu.Lock()
	hc.probes["done-stop"] = p
	hc.mu.Unlock()
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		hc.run(ctx, p)
	}()
	time.Sleep(100 * time.Millisecond)
	close(done)
	wg.Wait()
}
