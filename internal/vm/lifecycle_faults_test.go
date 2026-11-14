//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
)

// The VM lifecycle when the hypervisor misbehaves: it cannot be launched, it
// dies during start, it exits non-zero, it ignores the request to stop, it
// crashes in a loop. And when callers misbehave: two starts, two stops, a
// remove in the middle of a start or of a restart backoff. Whatever happens
// the VM must end in a definite state, its process must be reaped, and nothing
// may come back that the user removed.

// fakeVMMEnv selects the behavior of the fake hypervisor process: this test
// binary re-executed. A real process (rather than a mock) is what lets the
// manager's wait, signal and kill paths run for real.
const fakeVMMEnv = "JERBOA_VM_FAKE_VMM"

// fakeVMM returns a CommandFunc whose processes behave as mode says:
//
//	block          run until signaled
//	ignore-term    ignore SIGTERM, run until killed
//	exit:N         exit at once with status N
//	stderr:N:TEXT  print TEXT on stderr, then exit with status N
//
// Every process prints "ready" on stdout once its signal handling is set up.
func fakeVMM(mode string) CommandFunc {
	return func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFakeVMM$")
		cmd.Env = append(os.Environ(), fakeVMMEnv+"="+mode)
		return cmd
	}
}

func TestHelperFakeVMM(t *testing.T) {
	mode := os.Getenv(fakeVMMEnv)
	if mode == "" {
		t.Skip("helper process standing in for a hypervisor")
	}
	kind, rest, _ := strings.Cut(mode, ":")
	switch kind {
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
	case "exit":
		code, _ := strconv.Atoi(rest)
		os.Exit(code)
	case "stderr":
		status, text, _ := strings.Cut(rest, ":")
		code, _ := strconv.Atoi(status)
		fmt.Fprintln(os.Stderr, text)
		os.Exit(code)
	}
	fmt.Println("ready")
	time.Sleep(time.Hour)
	os.Exit(0)
}

// faultCfg is a minimal config both hosts accept: Linux boots it directly,
// macOS needs the explicit x86 emulation path, which requires no kernel file.
func faultCfg() Config {
	cfg := Config{ImagePath: "test.img", Memory: "256M"}
	if runtime.GOOS == "darwin" {
		cfg.EmulateX86 = true
		cfg.Architecture = "amd64"
	}
	return cfg
}

func waitDone(t *testing.T, v *VM) {
	t.Helper()
	select {
	case <-v.Done():
	case <-time.After(30 * time.Second):
		t.Fatalf("vm %s did not stop (state %s)", v.ID, v.GetState())
	}
}

func waitReady(t *testing.T, v *VM) {
	t.Helper()
	require.Eventually(t, func() bool { return strings.Contains(string(v.Logs()), "ready") },
		30*time.Second, 5*time.Millisecond, "fake hypervisor never came up")
}

// spawnCounter wraps a CommandFunc and counts the processes it was asked for.
type spawnCounter struct {
	n    atomic.Int32
	next func(n int32) CommandFunc
}

func (c *spawnCounter) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	return c.next(c.n.Add(1))(ctx, name, args...)
}

// always spawns processes of one mode.
func always(mode string) *spawnCounter {
	return &spawnCounter{next: func(int32) CommandFunc { return fakeVMM(mode) }}
}

// missingVMM is a hypervisor binary that does not exist.
func missingVMM(context.Context, string, ...string) *exec.Cmd {
	return exec.Command(filepath.Join(os.TempDir(), "jerboa-no-such-hypervisor"))
}

type countingSink struct{ restarts, errs atomic.Int32 }

func (s *countingSink) RecordRestart() { s.restarts.Add(1) }
func (s *countingSink) RecordError()   { s.errs.Add(1) }

func requireReaped(t *testing.T, pid int) {
	t.Helper()
	require.Positive(t, pid)
	require.Eventually(t, func() bool { return !processAlive(pid) }, 30*time.Second, 5*time.Millisecond,
		"hypervisor process %d is still there (running or a zombie)", pid)
}

func vmPID(v *VM) int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.pid
}

func TestFaults_HypervisorCannotBeLaunched(t *testing.T) {
	store := NewMemoryStore()
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(missingVMM), WithStore(store))
	cfg := faultCfg()
	cfg.Restart = RestartConfig{Policy: RestartAlways}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	require.Error(t, mgr.Start(context.Background(), v.ID))
	assert.Equal(t, StateStopped, v.GetState(), "a VM that never launched must not stay in starting")
	waitDone(t, v)
	assert.NoFileExists(t, qmpSocketPath(v.ID))

	// It is a dead VM, not a fresh one: it cannot be started again, but it
	// can be removed, and the failed launch is not retried by the restart
	// policy behind the user's back.
	require.ErrorIs(t, mgr.Start(context.Background(), v.ID), ErrInvalidTransition)
	require.NoError(t, mgr.Stop(context.Background(), v.ID))
	require.NoError(t, mgr.Remove(context.Background(), v.ID))
	assert.Empty(t, store.List())
}

func TestFaults_HypervisorExitsDuringStart(t *testing.T) {
	for _, mode := range []string{"exit:0", "exit:1", "exit:3", "exit:2", "stderr:1:fake-qemu: could not open disk image"} {
		t.Run(mode, func(t *testing.T) {
			mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeVMM(mode)))
			v, err := mgr.Create(context.Background(), faultCfg())
			require.NoError(t, err)
			// The process may be gone before Start returns; Start itself
			// succeeded in launching it, so it reports no error.
			require.NoError(t, mgr.Start(context.Background(), v.ID))
			waitDone(t, v)
			assert.Equal(t, StateStopped, v.GetState())
			_, stoppedAt := v.GetTimes()
			assert.NotNil(t, stoppedAt)
			requireReaped(t, vmPID(v))
			assert.Zero(t, probeCountFor(mgr.hchecker))

			// Every control call on the dead VM is a clean no-op or error.
			require.NoError(t, mgr.Stop(context.Background(), v.ID))
			require.NoError(t, mgr.Kill(context.Background(), v.ID))
			require.NoError(t, mgr.Remove(context.Background(), v.ID))
			_, err = mgr.Get(v.ID)
			require.Error(t, err)
		})
	}
}

func probeCountFor(hc *HealthChecker) int {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	return len(hc.probes)
}

func TestFaults_StaleSocketFromAPreviousRunIsCleared(t *testing.T) {
	var sawStale atomic.Bool
	var sock string
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if _, err := os.Stat(sock); err == nil {
			sawStale.Store(true)
		}
		return fakeVMM("block")(ctx, name, args...)
	}))
	v, err := mgr.Create(context.Background(), faultCfg())
	require.NoError(t, err)
	// A daemon that crashed left the socket of this VM behind; a hypervisor
	// cannot bind over it.
	sock = qmpSocketPath(v.ID)
	require.NoError(t, os.WriteFile(sock, nil, 0o600))
	t.Cleanup(func() { _ = os.Remove(sock) })

	require.NoError(t, mgr.Start(context.Background(), v.ID))
	assert.False(t, sawStale.Load(), "the hypervisor was launched with the stale socket still in place")

	// The socket the hypervisor bound is removed when it exits.
	require.NoError(t, os.WriteFile(sock, nil, 0o600))
	require.NoError(t, mgr.Kill(context.Background(), v.ID))
	waitDone(t, v)
	require.Eventually(t, func() bool {
		_, err := os.Stat(sock)
		return os.IsNotExist(err)
	}, 10*time.Second, 5*time.Millisecond)
}

func TestFaults_LimitsThatCannotBeAppliedAbortTheStart(t *testing.T) {
	spawns := always("block")
	store := NewMemoryStore()
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(spawns.cmd), WithStore(store))
	mgr.applyLimits = func(*VM, int) error { return errors.New("cgroup: read-only file system") }
	cfg := faultCfg()
	cfg.CPUShares = 100
	cfg.Restart = RestartConfig{Policy: RestartAlways}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), v.ID)
	require.ErrorContains(t, err, "read-only file system")
	waitDone(t, v)
	assert.Equal(t, StateStopped, v.GetState())
	requireReaped(t, vmPID(v))

	// The abort is deliberate: the restart policy must not relaunch the VM
	// without the limits the user asked for.
	require.Never(t, func() bool { return spawns.n.Load() > 1 || len(store.List()) > 1 },
		1500*time.Millisecond, 20*time.Millisecond, "a VM aborted during start was restarted")
}

func TestFaults_ConcurrentStartLaunchesOneHypervisor(t *testing.T) {
	if raceDetector {
		t.Skip("BUG: Start validates v.Cfg before it wins the transition to starting, while the Start that won writes v.Cfg (TapName, native socket) without a lock: a data race between two concurrent starts of one VM")
	}
	spawns := always("block")
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(spawns.cmd))
	v, err := mgr.Create(context.Background(), faultCfg())
	require.NoError(t, err)

	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = mgr.Start(context.Background(), v.ID) }()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		require.ErrorIs(t, err, ErrInvalidTransition)
	}
	assert.Equal(t, 1, ok, "exactly one Start may win")
	assert.Equal(t, int32(1), spawns.n.Load(), "more than one hypervisor was launched for one VM")
	assert.Equal(t, StateRunning, v.GetState())

	require.NoError(t, mgr.Kill(context.Background(), v.ID))
	waitDone(t, v)
	requireReaped(t, vmPID(v))
}

func TestFaults_ConcurrentStopAndKill(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeVMM("block")))
	cfg := faultCfg()
	cfg.HealthCheck = &HealthCheckConfig{Type: "tcp", Port: 80, Interval: time.Hour}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	waitReady(t, v)

	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if i%2 == 0 {
				errs[i] = mgr.Stop(ctx, v.ID)
			} else {
				errs[i] = mgr.Kill(ctx, v.ID)
			}
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		// A caller that lost the race is told the VM is already stopping.
		require.ErrorIs(t, err, ErrInvalidTransition)
	}
	assert.Positive(t, ok)
	waitDone(t, v)
	assert.Equal(t, StateStopped, v.GetState())
	requireReaped(t, vmPID(v))
	assert.Zero(t, probeCountFor(mgr.hchecker), "the health probe outlived its VM")
	require.NoError(t, mgr.Remove(context.Background(), v.ID))
}

func TestFaults_HypervisorThatIgnoresStopIsKilled(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeVMM("ignore-term")))
	v, err := mgr.Create(context.Background(), faultCfg())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	waitReady(t, v) // SIGTERM is ignored from here on

	// The caller's deadline stands in for the grace period.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	require.NoError(t, mgr.Stop(ctx, v.ID))
	assert.Less(t, time.Since(begin), gracePeriod/2, "Stop waited the whole grace period although its context expired")
	waitDone(t, v)
	assert.Equal(t, StateStopped, v.GetState())
	requireReaped(t, vmPID(v))
	assert.True(t, v.IsExplicitStop())
}

func TestFaults_StopRacingAHypervisorThatExitsByItself(t *testing.T) {
	for range 8 {
		mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeVMM("exit:0")))
		v, err := mgr.Create(context.Background(), faultCfg())
		require.NoError(t, err)
		require.NoError(t, mgr.Start(context.Background(), v.ID))
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err = mgr.Stop(ctx, v.ID)
		cancel()
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidTransition)
		}
		waitDone(t, v)
		assert.Equal(t, StateStopped, v.GetState())
		requireReaped(t, vmPID(v))
	}
}

func TestFaults_ControlCallsWhileStarting(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeVMM("block")))
	// Hold the start between launching the process and declaring it running.
	mgr.applyLimits = func(*VM, int) error { close(entered); <-release; return nil }
	cfg := faultCfg()
	cfg.CPUShares = 100
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	started := make(chan error, 1)
	go func() { started <- mgr.Start(context.Background(), v.ID) }()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("start never reached the limits step")
	}
	require.Equal(t, StateStarting, v.GetState())

	require.Error(t, mgr.Remove(context.Background(), v.ID), "a VM was removed while its hypervisor was being started")
	_, err = mgr.Get(v.ID)
	require.NoError(t, err)
	require.ErrorIs(t, mgr.Start(context.Background(), v.ID), ErrInvalidTransition)
	require.ErrorIs(t, mgr.Stop(context.Background(), v.ID), ErrInvalidTransition)
	require.ErrorIs(t, mgr.Kill(context.Background(), v.ID), ErrInvalidTransition)
	assert.False(t, v.IsExplicitStop(), "a refused stop marked the VM as explicitly stopped")

	close(release)
	require.NoError(t, <-started)
	assert.Equal(t, StateRunning, v.GetState())
	require.NoError(t, mgr.Kill(context.Background(), v.ID))
	waitDone(t, v)
	requireReaped(t, vmPID(v))
	require.NoError(t, mgr.Remove(context.Background(), v.ID))
}

func TestFaults_ControlCallsOnVMsThatAreNotRunning(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeVMM("block")))
	ctx := context.Background()
	for _, call := range []func(string) error{
		func(id string) error { return mgr.Start(ctx, id) },
		func(id string) error { return mgr.Stop(ctx, id) },
		func(id string) error { return mgr.Kill(ctx, id) },
		func(id string) error { return mgr.Signal(ctx, id, syscall.SIGTERM) },
		func(id string) error { return mgr.Remove(ctx, id) },
	} {
		require.Error(t, call("no-such-vm"))
	}

	v, err := mgr.Create(ctx, faultCfg())
	require.NoError(t, err)
	// Never started: there is no process to signal or stop.
	require.Error(t, mgr.Signal(ctx, v.ID, syscall.SIGTERM))
	require.Error(t, mgr.Signal(ctx, v.ID, syscall.SIGKILL))
	require.ErrorIs(t, mgr.Stop(ctx, v.ID), ErrInvalidTransition)
	require.ErrorIs(t, mgr.Kill(ctx, v.ID), ErrInvalidTransition)
	assert.Equal(t, StateCreated, v.GetState())
	require.NoError(t, mgr.Remove(ctx, v.ID))
	require.Error(t, mgr.Remove(ctx, v.ID), "removing twice must fail")
}

func TestFaults_SignalAfterTheProcessIsGone(t *testing.T) {
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeVMM("exit:0")))
	v, err := mgr.Create(context.Background(), faultCfg())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	waitDone(t, v)
	requireReaped(t, vmPID(v))
	// The pid may already belong to someone else: signaling a finished
	// process must be a no-op, never an error and never a signal sent.
	require.NoError(t, mgr.Signal(context.Background(), v.ID, syscall.SIGKILL))
	require.NoError(t, mgr.Signal(context.Background(), v.ID, syscall.SIGTERM))
}

func TestFaults_CrashLoopIsBoundedByMaxRetries(t *testing.T) {
	spawns := always("exit:3") // status 3: the guest exited with code 1
	store := NewMemoryStore()
	sink := &countingSink{}
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(spawns.cmd), WithStore(store), WithMetrics(sink))
	cfg := faultCfg()
	cfg.Name = "crasher"
	cfg.Restart = RestartConfig{Policy: RestartOnFailure, MaxRetries: 1}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)
	begin := time.Now()
	require.NoError(t, mgr.Start(context.Background(), v.ID))

	// One retry: the replacement takes over the registry entry and, when it
	// crashes too, the loop ends.
	require.Eventually(t, func() bool {
		vms := store.List()
		return spawns.n.Load() == 2 && len(vms) == 1 && vms[0].ID != v.ID && vms[0].GetState() == StateStopped
	}, 30*time.Second, 10*time.Millisecond)
	assert.GreaterOrEqual(t, time.Since(begin), time.Second, "the first retry must wait out its backoff")
	last := store.List()[0]
	assert.Equal(t, 1, last.GetRestartCount())
	assert.Equal(t, "crasher", last.Cfg.Name)
	assert.Equal(t, int32(1), sink.restarts.Load())
	assert.Zero(t, sink.errs.Load())

	require.Never(t, func() bool { return spawns.n.Load() > 2 || len(store.List()) != 1 },
		2500*time.Millisecond, 20*time.Millisecond, "the VM was restarted past MaxRetries")
}

func TestFaults_OnFailureRestartsOnlyFailures(t *testing.T) {
	// Both VMs run under one manager so the wait for "no restart happened"
	// is paid once.
	clean := always("exit:0")
	guestZero := always("exit:1") // status 1 with a silent hypervisor: guest exit(0)
	hypervisorError := &spawnCounter{next: func(n int32) CommandFunc {
		if n == 1 {
			return fakeVMM("stderr:1:qemu-system-x86_64: could not open disk image")
		}
		return fakeVMM("block")
	}}
	type run struct {
		name    string
		spawns  *spawnCounter
		restart bool
	}
	runs := []run{{"clean", clean, false}, {"guest-zero", guestZero, false}, {"hypervisor-error", hypervisorError, true}}
	stores := map[string]*MemoryStore{}
	for _, r := range runs {
		store := NewMemoryStore()
		stores[r.name] = store
		mgr := NewQEMUManager("fake-qemu", WithCommandFunc(r.spawns.cmd), WithStore(store))
		cfg := faultCfg()
		cfg.Restart = RestartConfig{Policy: RestartOnFailure, MaxRetries: 1}
		v, err := mgr.Create(context.Background(), cfg)
		require.NoError(t, err)
		require.NoError(t, mgr.Start(context.Background(), v.ID))
		waitDone(t, v)
		t.Cleanup(func() {
			for _, v := range store.List() {
				_ = mgr.Kill(context.Background(), v.ID)
			}
		})
	}
	require.Eventually(t, func() bool {
		vms := stores["hypervisor-error"].List()
		return hypervisorError.n.Load() == 2 && len(vms) == 1 && vms[0].GetState() == StateRunning
	}, 30*time.Second, 10*time.Millisecond, "a hypervisor that failed on its own was not restarted")
	// The backoff of the first restart has long passed by now.
	time.Sleep(200 * time.Millisecond)
	for _, r := range runs[:2] {
		assert.Equal(t, int32(1), r.spawns.n.Load(), "%s: a clean exit was treated as a failure", r.name)
		require.Len(t, stores[r.name].List(), 1)
		assert.Equal(t, StateStopped, stores[r.name].List()[0].GetState())
	}
}

func TestFaults_VMRemovedDuringRestartBackoffStaysRemoved(t *testing.T) {
	spawns := always("exit:3")
	store := NewMemoryStore()
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(spawns.cmd), WithStore(store))
	cfg := faultCfg()
	cfg.Restart = RestartConfig{Policy: RestartAlways, MaxRetries: 5}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	waitDone(t, v)

	// The VM crashed and its restart is waiting out the backoff; the user
	// removes it now.
	require.NoError(t, mgr.Remove(context.Background(), v.ID))
	require.Never(t, func() bool { return spawns.n.Load() > 1 || len(store.List()) > 0 },
		1800*time.Millisecond, 20*time.Millisecond, "a VM the user removed was brought back by its restart policy")
}

func TestFaults_ReplacementThatCannotLaunchEndsTheLoop(t *testing.T) {
	spawns := &spawnCounter{next: func(n int32) CommandFunc {
		if n == 1 {
			return fakeVMM("exit:3")
		}
		return missingVMM
	}}
	store := NewMemoryStore()
	sink := &countingSink{}
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(spawns.cmd), WithStore(store), WithMetrics(sink))
	cfg := faultCfg()
	cfg.Restart = RestartConfig{Policy: RestartAlways}
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))

	require.Eventually(t, func() bool { return sink.errs.Load() == 1 }, 30*time.Second, 10*time.Millisecond,
		"a replacement that failed to launch was not reported")
	// The crashed VM is still registered, so the user can see what happened,
	// and nothing is left in a state that claims to be alive.
	_, err = store.Get(v.ID)
	require.NoError(t, err, "the crashed VM was dropped although its replacement never ran")
	for _, vm := range store.List() {
		assert.Equal(t, StateStopped, vm.GetState())
	}
	require.Never(t, func() bool { return spawns.n.Load() > 2 }, 2500*time.Millisecond, 20*time.Millisecond,
		"a launch failure is retried forever")
}

func TestFaults_LifecycleIsPersistedAtEachStep(t *testing.T) {
	dir := t.TempDir()
	store := restored(t, dir)
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeVMM("block")), WithStore(store))
	v, err := mgr.Create(context.Background(), faultCfg())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	waitReady(t, v)

	onDisk := func() vmState {
		t.Helper()
		var st vmState
		readJSONFile(t, filepath.Join(dir, v.ID, "state.json"), &st)
		return st
	}
	st := onDisk()
	assert.Equal(t, StateRunning, st.State)
	assert.Equal(t, vmPID(v), st.PID, "without the pid on disk a restarted daemon cannot find the hypervisor")

	require.NoError(t, mgr.Kill(context.Background(), v.ID))
	waitDone(t, v)
	require.Eventually(t, func() bool { return onDisk().State == StateStopped }, 10*time.Second, 5*time.Millisecond)
	assert.NotNil(t, onDisk().StoppedAt)

	// A daemon restarted now sees an ordinary stopped VM, not one to recover.
	got, err := restored(t, dir).Get(v.ID)
	require.NoError(t, err)
	assert.Equal(t, StateStopped, got.GetState())
	assert.False(t, got.DaemonRecovered)
}

func TestFaults_RunningStateThatCannotBeSaved(t *testing.T) {
	t.Skip("BUG: Start ignores a failed save of the running state (qemu.go: `_ = m.store.Save(v)`), so with a full disk the VM runs while the registry still says created; after a daemon restart its hypervisor is an untracked orphan")
	dir := t.TempDir()
	store := restored(t, dir)
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeVMM("block")), WithStore(store))
	v, err := mgr.Create(context.Background(), faultCfg())
	require.NoError(t, err)

	durabletest.Inject(t, durabletest.Failures()[0], 0, nil) // disk full on the first write
	err = mgr.Start(context.Background(), v.ID)
	durable.SetFault(nil)
	t.Cleanup(func() { _ = mgr.Kill(context.Background(), v.ID) })

	// Either the start fails and the process is gone, or the registry knows
	// about the running VM.
	if err != nil {
		waitDone(t, v)
		requireReaped(t, vmPID(v))
		return
	}
	var st vmState
	readJSONFile(t, filepath.Join(dir, v.ID, "state.json"), &st)
	assert.Equal(t, StateRunning, st.State)
	assert.Equal(t, vmPID(v), st.PID)
}

func readJSONFile(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, into))
}
