//go:build linux

package vm

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
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

// With no private runtime dir to put the QMP socket in, Start refuses the VM
// instead of using a directory someone else controls.
func TestQEMUManager_Start_NoPrivateRuntimeDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores the read-only temp dir this test relies on")
	}
	runtimeDir.mu.Lock()
	saved := runtimeDir.path
	runtimeDir.path = ""
	runtimeDir.mu.Unlock()
	t.Cleanup(func() {
		runtimeDir.mu.Lock()
		runtimeDir.path = saved
		runtimeDir.mu.Unlock()
	})
	base := t.TempDir()
	require.NoError(t, os.Symlink(t.TempDir(), expectedRuntimeDir(base, os.Getuid())))
	require.NoError(t, os.Chmod(base, 0o500))
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) })
	t.Setenv("TMPDIR", base)

	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	err = mgr.Start(context.Background(), v.ID)
	require.ErrorContains(t, err, "runtime dir")
	require.Equal(t, StateCreated, v.GetState())

	// Nothing was cached: once the temp dir is usable again, Start works.
	require.NoError(t, os.Chmod(base, 0o700))
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.NoError(t, mgr.Stop(context.Background(), v.ID))
	<-v.Done()
}

// An attach-mode VM whose console nobody reads is still reaped when the VMM
// exits: the guest's output must not block the monitor.
func TestQEMUManager_AttachWithoutReader_StillStops(t *testing.T) {
	// Far more output than any pipe buffer, then a clean exit.
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(func(context.Context, string, ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "head -c 4000000 /dev/zero; echo last-line; exit 0")
	}))
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M", Attach: true})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	select {
	case <-v.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("attach-mode VM never reached stopped with no console reader")
	}
	require.Equal(t, StateStopped, v.GetState())

	// A client attaching late still gets the newest output and the end.
	got, err := io.ReadAll(v.AttachSession().Reader)
	require.NoError(t, err)
	require.LessOrEqual(t, len(got), attachPipeMaxBytes)
	require.True(t, strings.HasSuffix(string(got), "last-line\n"))
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
	// Use a blocking fake process so the VM stays running until we explicitly
	// stop it. A non-blocking fake exits on its own, which the monitor treats as
	// a crash and — under RestartAlways with no retry cap — restarts forever,
	// leaking a perpetual chain of backoff-sleeping restart goroutines into
	// later tests (which -shuffle=on then attributes to an unrelated test's
	// goleak check).
	mgr := fakeManager(true)
	ctx := context.Background()
	v, err := mgr.Create(ctx, Config{
		ImagePath: "test.img",
		Memory:    "256M",
		Restart:   RestartConfig{Policy: RestartAlways},
	})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx, v.ID))
	// Safety net: if the explicit Stop below errors or the Done wait times out,
	// the test aborts while this blocking VM is still under RestartAlways (no
	// retry cap), which would leak restart goroutines into later shuffled tests.
	// Stop it on the way out unless it already stopped.
	t.Cleanup(func() {
		select {
		case <-v.Done():
			return
		default:
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = mgr.Stop(cleanupCtx, v.ID)
	})

	// An explicit Stop sets the explicit-stop flag the monitor checks, so the
	// RestartAlways policy is overridden and no replacement is spawned.
	require.NoError(t, mgr.Stop(ctx, v.ID))
	select {
	case <-v.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("VM did not stop")
	}
	require.Equal(t, StateStopped, v.GetState())
	require.Equal(t, 0, v.GetRestartCount(), "explicitly stopped VM must not restart")
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
			// A guest crash: with isa-debug-exit QEMU exits (guestCode<<1)|1, so a
			// guest exit(3) surfaces as process status 7. on-failure must restart.
			return guestExitCmd(3)
		}
		// A clean guest exit(0) surfaces as process status 1; on-failure must NOT
		// restart again, so the chain settles after the single restart above.
		return guestExitCmd(0)
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
	// Scope the leak check to the run goroutine this test starts below, so a
	// goroutine leaked by an unrelated test under -shuffle=on is not
	// misattributed here.
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	hc := NewHealthChecker()
	done := make(chan struct{})
	p := &healthProbe{
		vm: &VM{
			ID:    "done-stop",
			Cfg:   Config{PortMaps: []PortMap{{HostPort: 1, GuestPort: 80, Protocol: ProtocolTCP}}},
			State: StateRunning,
			done:  make(chan struct{}),
		},
		cfg:  HealthCheckConfig{Type: "tcp", Port: 1, Interval: 10 * time.Millisecond, Timeout: 10 * time.Millisecond, Retries: 1},
		done: done,
	}
	hc.mu.Lock()
	hc.probes["done-stop"] = p
	hc.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		hc.run(context.Background(), p)
	}()

	// Closing the probe's done channel must stop the run goroutine. The explicit
	// timeout turns a stuck goroutine into a clear failure instead of a hang.
	close(done)
	waitOrTimeout(t, &wg, 2*time.Second)
}
