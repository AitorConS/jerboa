//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A daemon restart with hypervisors that outlived it, died with it, or whose
// pid now belongs to someone else. A surviving hypervisor must be adopted and
// stay controllable; a pid that is not provably the VM's hypervisor must
// never be adopted, signaled or killed.

// survivingHypervisor starts a process whose command line is that of the
// hypervisor of VM id, as left running by a daemon that died. reap waits for
// it, which is what turns a killed child from a zombie into a dead pid.
func survivingHypervisor(t *testing.T, id string) (pid int, reap func()) {
	t.Helper()
	// Both platforms recognize their hypervisor by the VM's QMP socket on its
	// command line; macOS also requires the binary name.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFakeVMM$", "--",
		"qemu-system-aarch64", "-qmp", "unix:"+qmpSocketPath(id)+",server,nowait")
	cmd.Env = append(os.Environ(), fakeVMMEnv+"=block")
	require.NoError(t, cmd.Start())
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})
	require.Eventually(t, func() bool { return processOwnsVM(cmd.Process.Pid, id) }, 30*time.Second, 10*time.Millisecond,
		"the fake hypervisor is not recognized as VM %s's process", id)
	return cmd.Process.Pid, func() { <-waited }
}

// recordedAs persists a VM in state with pid, as the previous daemon left it,
// in a registry of the given kind. reopenStore opens that registry again the
// way a starting daemon does; onDisk reads the persisted state and recovered
// marker without running any recovery.
func recordedAs(t *testing.T, kind string, state State, pid func(id string) int) (id string, reopenStore func() Store, onDisk func(cur Store) (State, bool)) {
	t.Helper()
	dir := t.TempDir()
	var first Store
	switch kind {
	case "file":
		first = restored(t, dir)
		reopenStore = func() Store { return restored(t, dir) }
		onDisk = func(Store) (State, bool) {
			var st vmState
			readJSONFile(t, filepath.Join(dir, id, "state.json"), &st)
			return st.State, st.DaemonRecovered
		}
	case "sqlite":
		path := filepath.Join(dir, "vms.db")
		s := restoredSQLite(t, path)
		first = s
		t.Cleanup(func() { _ = s.Close() })
		reopenStore = func() Store { return restoredSQLite(t, path) }
		// Read through the store's own connection: a second connection
		// could hold the lock at the moment the monitor saves.
		onDisk = func(cur Store) (State, bool) {
			var st string
			var recovered int
			err := cur.(*SQLiteStore).db.QueryRow("SELECT state, daemon_recovered FROM vms WHERE id = ?", id).Scan(&st, &recovered)
			require.NoError(t, err)
			return State(st), recovered != 0
		}
	}
	v, err := first.Create(Config{Name: "app", ImagePath: "a.img", Memory: "256M"})
	require.NoError(t, err)
	v.mu.Lock()
	v.State = state
	v.pid = pid(v.ID)
	v.mu.Unlock()
	require.NoError(t, first.Save(v))
	if c, ok := first.(*SQLiteStore); ok {
		require.NoError(t, c.Close())
	}
	id = v.ID
	return id, reopenStore, onDisk
}

func TestFaults_SurvivingHypervisorIsAdoptedAndStaysControllable(t *testing.T) {
	for _, kind := range []string{"file", "sqlite"} {
		for _, state := range []State{StateRunning, StateStarting, StateStopping} {
			t.Run(kind+"/"+string(state), func(t *testing.T) {
				var pid int
				var reap func()
				id, reopenStore, onDisk := recordedAs(t, kind, state, func(id string) int {
					pid, reap = survivingHypervisor(t, id)
					return pid
				})

				store := reopenStore()
				v, err := store.Get(id)
				require.NoError(t, err)
				assert.Equal(t, StateRunning, v.GetState(), "a VM whose hypervisor survived was not adopted")
				assert.False(t, v.DaemonRecovered)
				assert.Equal(t, pid, vmPID(v))
				select {
				case <-v.Done():
					t.Fatal("done closed for a running VM")
				default:
				}
				assert.True(t, processAlive(pid), "adopting a hypervisor must not disturb it")

				// The adopted VM obeys the new daemon: it can be killed, and
				// its exit is noticed and recorded.
				mgr := NewQEMUManager("fake-qemu", WithStore(store))
				require.Error(t, mgr.Remove(context.Background(), id), "a running adopted VM was removed")
				require.NoError(t, mgr.Kill(context.Background(), id))
				reap()
				waitDone(t, v)
				assert.Equal(t, StateStopped, v.GetState())

				// The poll loop saves right after it closes done.
				require.Eventually(t, func() bool {
					st, recovered := onDisk(store)
					return st == StateStopped && !recovered
				}, 10*time.Second, 20*time.Millisecond, "the exit of an adopted VM was not persisted")
			})
		}
	}
}

func TestFaults_PIDThatIsNotTheHypervisorIsNeverAdopted(t *testing.T) {
	otherVM := "ffffffffffff"
	otherPID, _ := survivingHypervisor(t, otherVM)
	pids := map[string]func(id string) int{
		"no pid recorded": func(string) int { return 0 },
		"negative pid":    func(string) int { return -1 },
		"dead pid":        func(string) int { return 0x7ffffff0 },
		// The pid was reused by an unrelated process: this test.
		"pid reused": func(string) int { return os.Getpid() },
		// The pid now belongs to the hypervisor of another VM.
		"another VM's hypervisor": func(string) int { return otherPID },
		"init":                    func(string) int { return 1 },
	}
	for name, pid := range pids {
		for _, kind := range []string{"file", "sqlite"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				id, reopenStore, _ := recordedAs(t, kind, StateRunning, pid)
				store := reopenStore()
				v, err := store.Get(id)
				require.NoError(t, err)
				assert.Equal(t, StateStopped, v.GetState(), "a process that is not this VM's hypervisor was adopted")
				assert.True(t, v.DaemonRecovered)
				waitDone(t, v)
				v.mu.RLock()
				proc := v.proc
				v.mu.RUnlock()
				assert.Nil(t, proc, "the VM holds a handle on a process that is not its own")

				// Control calls on the recovered VM reach no process.
				mgr := NewQEMUManager("fake-qemu", WithStore(store))
				require.NoError(t, mgr.Kill(context.Background(), id))
				require.NoError(t, mgr.Stop(context.Background(), id))
				assert.True(t, processAlive(otherPID), "recovering one VM killed another VM's hypervisor")
				require.NoError(t, mgr.Remove(context.Background(), id))
			})
		}
	}
}

func TestFaults_ProcessAlive(t *testing.T) {
	assert.False(t, processAlive(0))
	assert.False(t, processAlive(-1), "pid -1 addresses every process and must never count as alive")
	assert.False(t, processAlive(0x7ffffff0))
	assert.True(t, processAlive(os.Getpid()))
	// A process this user may not signal exists all the same.
	assert.True(t, processAlive(1))

	cmd := fakeVMM("exit:0")(context.Background(), "")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	assert.False(t, processAlive(pid), "a reaped process still counts as alive")
}

func TestFaults_AdoptMonitorCleansUpWhenTheProcessIsAlreadyGone(t *testing.T) {
	store := NewMemoryStore()
	v, err := store.Create(Config{Name: "app", ImagePath: "a.img"})
	require.NoError(t, err)
	sock := filepath.Join(t.TempDir(), "qmp.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))
	var cleaned atomic.Int32
	v.mu.Lock()
	v.State = StateRunning
	v.pid = 0x7ffffff0 // died between adoption and the first poll
	v.qmpAddr = "unix:" + sock
	v.hostCleanup = func() { cleaned.Add(1) }
	v.mu.Unlock()

	go adoptMonitor(store, v)
	waitDone(t, v)
	assert.Equal(t, StateStopped, v.GetState())
	assert.Equal(t, int32(1), cleaned.Load(), "host resources of the dead VM were not released exactly once")
	assert.NoFileExists(t, sock)
}

func TestFaults_AdoptMonitorSurvivesAVMThatWasStoppedMeanwhile(t *testing.T) {
	store := NewMemoryStore()
	v, err := store.Create(Config{Name: "app", ImagePath: "a.img"})
	require.NoError(t, err)
	v.mu.Lock()
	v.State = StateStopped // something else already recorded the stop
	v.pid = 0x7ffffff0
	close(v.done)
	v.mu.Unlock()

	finished := make(chan struct{})
	go func() { adoptMonitor(store, v); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the adopt monitor never returned")
	}
	// A second close of done would have panicked; the state is unchanged.
	assert.Equal(t, StateStopped, v.GetState())
}

func TestFaults_BackgroundPanicDoesNotEscape(t *testing.T) {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer recoverGoroutine("test goroutine", "abc")
		var v *VM
		_ = v.Cfg.Name // nil dereference, as a teardown bug would cause
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutine did not finish")
	}
}

func TestFaults_ReconcileNetworkWithoutANetworkTouchesNothing(t *testing.T) {
	// A VM without a network has nothing to detach; this must not shell out
	// or fail for lack of privileges.
	reconcileNetwork("0123456789ab", Config{})
}

// seedGuestEnvFile leaves a guest environment file for id, as a VM started
// with -e by a daemon that then died would have.
func seedGuestEnvFile(t *testing.T, id string) string {
	t.Helper()
	path := guestEnvFilePath(id)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("API_TOKEN=s3cr3t"), 0o600))
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

// A restarted daemon that finds the VM dead removes the secrets it was
// started with.
func TestFaults_RecoverRemovesTheEnvFileOfADeadVM(t *testing.T) {
	store := NewMemoryStore()
	v, err := store.Create(Config{Name: "app", ImagePath: "a.img"})
	require.NoError(t, err)
	path := seedGuestEnvFile(t, v.ID)

	require.True(t, recoverVM(store, v, 0x7ffffff0))
	assert.Equal(t, StateStopped, v.GetState())
	assert.NoFileExists(t, path)
}

// An adopted VM that exits later leaves no secrets behind either.
func TestFaults_AdoptMonitorRemovesTheEnvFile(t *testing.T) {
	store := NewMemoryStore()
	v, err := store.Create(Config{Name: "app", ImagePath: "a.img"})
	require.NoError(t, err)
	path := seedGuestEnvFile(t, v.ID)
	v.mu.Lock()
	v.State = StateRunning
	v.pid = 0x7ffffff0 // exited after it was adopted
	v.qmpAddr = "unix:" + filepath.Join(t.TempDir(), "qmp.sock")
	v.mu.Unlock()

	go adoptMonitor(store, v)
	waitDone(t, v)
	assert.NoFileExists(t, path)
}
