//go:build linux || (darwin && arm64)

package vm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The small pieces every failure path leans on: how an exit status is read,
// who owns a VM while an operation runs, a control socket that misbehaves, a
// guest that floods its log, input that is not what the parser expects.

// --- Exit status -----------------------------------------------------------

// exitWith runs a real process that ends with status, or is killed when
// status is negative, and returns what Wait reports.
func exitWith(t *testing.T, status int) error {
	t.Helper()
	if status < 0 {
		cmd := exec.Command("sleep", "3600")
		require.NoError(t, cmd.Start())
		require.NoError(t, cmd.Process.Kill())
		return cmd.Wait()
	}
	return exec.Command("sh", "-c", fmt.Sprintf("exit %d", status)).Run()
}

func TestFaults_GuestExitCodeFromHypervisorStatus(t *testing.T) {
	cases := []struct {
		status   int
		wantCode int
		wantOK   bool
	}{
		{0, 0, false}, // no debug-exit write at all
		{1, 0, true},  // guest exit(0)
		{3, 1, true},  // guest exit(1)
		{7, 3, true},  // guest exit(3)
		{255, 127, true},
		{2, 0, false}, // even: not a guest code, the hypervisor's own status
		{126, 0, false},
		{-1, 0, false}, // killed by a signal
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			code, ok := guestExitCode(exitWith(t, tc.status))
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantCode, code)
		})
	}
	// Errors that are not an exit status at all (the wait itself failed).
	for _, err := range []error{errors.New("wait: no child processes"), context.DeadlineExceeded, os.ErrProcessDone} {
		code, ok := guestExitCode(err)
		assert.False(t, ok, "%v", err)
		assert.Zero(t, code)
	}
}

func TestFaults_WhatCountsAsAFailedExit(t *testing.T) {
	const diskError = "qemu-system-x86_64: -drive file=/x.img: Could not open '/x.img': No such file or directory\n"
	cases := []struct {
		name   string
		status int
		stderr string
		want   bool
	}{
		{"clean process exit", 0, "", false},
		{"clean process exit with noise on stderr", 0, diskError, false},
		{"guest exit(0)", 1, "", false},
		{"guest exit(0) with a warning", 1, "qemu-system-x86_64: warning: host doesn't support requested feature\n", false},
		{"guest exit(0) with blank lines", 1, "\n\n  \n", false},
		{"guest exit(0) with unrelated text", 1, "some library: deprecated option\n", false},
		{"hypervisor failed on its own", 1, diskError, true},
		{"hypervisor error after a warning", 1, "qemu-system-x86_64: warning: x\n" + diskError, true},
		{"hypervisor error, upper case", 1, "QEMU-SYSTEM-X86_64: Could not set up host forwarding rule\n", true},
		{"guest crashed", 3, "", true},
		{"guest crashed, quiet hypervisor warning", 7, "qemu-system-x86_64: warning: x\n", true},
		{"hypervisor even status", 2, "", true},
		{"killed", -1, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isFailureExit(exitWith(t, tc.status), tc.stderr))
		})
	}
	assert.True(t, isFailureExit(errors.New("wait failed"), ""), "an exit that cannot be read is a failure")
	assert.False(t, isFailureExit(nil, ""))
}

// --- Lifecycle claims --------------------------------------------------------

func TestFaults_ConcurrentClaimsHaveOneOwner(t *testing.T) {
	var c lifecycleClaims
	const callers = 32
	var wg sync.WaitGroup
	var won atomic.Int32
	winner := make(chan string, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op := fmt.Sprintf("op-%d", i)
			if err := c.claim("vm", op); err != nil {
				assert.ErrorIs(t, err, ErrVMBusy)
				return
			}
			won.Add(1)
			winner <- op
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), won.Load())
	owner := <-winner
	assert.Equal(t, owner, c.current("vm"))

	// Only the owner can release: a loser's deferred release is harmless.
	c.release("vm", "someone-else")
	require.ErrorIs(t, c.claim("vm", "next"), ErrVMBusy)
	c.release("vm", owner)
	c.release("vm", owner)
	require.NoError(t, c.claim("vm", "next"))
	// A stale release from the previous owner does not free the new claim.
	c.release("vm", owner)
	assert.Equal(t, "next", c.current("vm"))
	// Claims are per VM.
	require.NoError(t, c.claim("other", "next"))
}

func TestFaults_RestartPendingMarker(t *testing.T) {
	var c lifecycleClaims
	assert.False(t, c.restartPending("vm"))
	c.setRestartPending("vm", false) // clearing what was never set
	c.setRestartPending("vm", true)
	c.setRestartPending("vm", true)
	assert.True(t, c.restartPending("vm"))
	assert.False(t, c.restartPending("other"))
	// The marker is independent of the claim: it is visible during the
	// backoff, before the restart claims the VM.
	require.NoError(t, c.claim("vm", opSnapshotCreate))
	assert.True(t, c.restartPending("vm"))
	c.setRestartPending("vm", false)
	assert.False(t, c.restartPending("vm"))
	assert.Equal(t, opSnapshotCreate, c.current("vm"))
}

func TestFaults_ClaimVMReleasesWhatItCannotUse(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "kernel")
	_, _, err := m.claimVM("no-such-vm", opRestart)
	require.Error(t, err)

	v, err := m.store.Create(Config{Name: "app"})
	require.NoError(t, err)
	// A VM in the middle of an in-place restore belongs to that restore even
	// after a daemon restart forgot the in-memory claim.
	v.mu.Lock()
	v.State = StateRestoring
	v.mu.Unlock()
	_, _, err = m.claimVM(v.ID, opRestart)
	require.ErrorIs(t, err, ErrVMBusy)
	assert.Empty(t, m.claims.current(v.ID), "a refused claim was left in place and would block the VM forever")

	v.mu.Lock()
	v.State = StateStopped
	v.mu.Unlock()
	got, release, err := m.claimVM("app", opRestart)
	require.NoError(t, err, "claiming by name")
	assert.Same(t, v, got)
	_, _, err = m.claimVM(v.ID, opSnapshotRestore)
	require.ErrorIs(t, err, ErrVMBusy)
	release()
	release()
	_, release, err = m.claimVM(v.ID, opSnapshotRestore)
	require.NoError(t, err)
	release()
}

func TestFaults_BeginRestoreOnlyFromStopped(t *testing.T) {
	for _, state := range []State{StateCreated, StateStarting, StateRunning, StateStopping, StateRestoring} {
		done := make(chan struct{})
		v := &VM{ID: "vm", State: state, done: done, pid: 42, explicitStop: true}
		require.ErrorIs(t, v.beginRestore(), ErrInvalidTransition, string(state))
		assert.Equal(t, state, v.GetState())
		// A refused restore must not swap the channel waiters hold, nor
		// forget the process of a VM that is running.
		assert.Equal(t, (<-chan struct{})(done), v.Done(), "%s: done channel replaced by a refused restore", state)
		assert.Equal(t, 42, v.pid)
		assert.True(t, v.explicitStop)
	}
}

func TestFaults_TransitionsUnderContention(t *testing.T) {
	// Many monitors and callers report the same VM stopped at once: exactly
	// one transition wins and done is closed exactly once (a second close
	// would panic).
	for range 50 {
		v := &VM{ID: "vm", State: StateRunning, done: make(chan struct{})}
		var wg sync.WaitGroup
		var won atomic.Int32
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if v.transition(StateStopped) == nil {
					won.Add(1)
				}
			}()
		}
		wg.Wait()
		require.Equal(t, int32(1), won.Load())
		<-v.Done()
	}
}

// --- QMP control socket ------------------------------------------------------

// qmpServer serves each connection on a Unix socket with handle and returns
// the QMP address of the socket.
func qmpServer(t *testing.T, handle func(c net.Conn, r *bufio.Reader)) string {
	t.Helper()
	// Unix socket paths are short; the default test directory is too deep.
	dir, err := os.MkdirTemp("", "qmp")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				handle(c, bufio.NewReader(c))
			}()
		}
	}()
	return "unix:" + path
}

const qmpGreeting = `{"QMP":{"version":{"qemu":{"major":9}},"capabilities":[]}}` + "\n"

func TestFaults_QMPPeerThatMisbehaves(t *testing.T) {
	peers := map[string]func(c net.Conn, r *bufio.Reader){
		"hangs up at once": func(net.Conn, *bufio.Reader) {},
		"greets with garbage": func(c net.Conn, _ *bufio.Reader) {
			_, _ = c.Write([]byte("\x00\xff not json\n"))
		},
		"hangs up after the greeting": func(c net.Conn, _ *bufio.Reader) {
			_, _ = c.Write([]byte(qmpGreeting))
		},
		"hangs up after negotiating": func(c net.Conn, r *bufio.Reader) {
			_, _ = c.Write([]byte(qmpGreeting))
			_, _ = r.ReadString('\n')
			_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
			_, _ = r.ReadString('\n')
		},
		"answers half a reply": func(c net.Conn, r *bufio.Reader) {
			_, _ = c.Write([]byte(qmpGreeting))
			_, _ = r.ReadString('\n')
			_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
			_, _ = r.ReadString('\n')
			_, _ = c.Write([]byte(`{"retu`))
		},
		"rejects the command": func(c net.Conn, r *bufio.Reader) {
			_, _ = c.Write([]byte(qmpGreeting))
			_, _ = r.ReadString('\n')
			_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
			_, _ = r.ReadString('\n')
			_, _ = c.Write([]byte(`{"error":{"class":"GenericError","desc":"guest is not running"}}` + "\n"))
		},
		"only ever sends events": func(c net.Conn, r *bufio.Reader) {
			_, _ = c.Write([]byte(qmpGreeting))
			_, _ = r.ReadString('\n')
			_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
			_, _ = r.ReadString('\n')
			for range 3 {
				_, _ = c.Write([]byte(`{"event":"RTC_CHANGE","data":{}}` + "\n"))
			}
		},
	}
	for name, handle := range peers {
		t.Run(name, func(t *testing.T) {
			addr := qmpServer(t, handle)
			done := make(chan error, 1)
			go func() { done <- qmpDo(addr, "system_powerdown") }()
			select {
			case err := <-done:
				require.Error(t, err, "a command that was never acknowledged was reported as delivered")
				if name == "rejects the command" {
					require.ErrorContains(t, err, "guest is not running")
				}
			case <-time.After(20 * time.Second):
				t.Fatal("qmpDo hangs on a misbehaving peer")
			}
		})
	}
}

func TestFaults_QMPReplyAfterEvents(t *testing.T) {
	var got atomic.Value
	addr := qmpServer(t, func(c net.Conn, r *bufio.Reader) {
		_, _ = c.Write([]byte(qmpGreeting))
		_, _ = r.ReadString('\n')
		_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
		line, _ := r.ReadString('\n')
		got.Store(line)
		// Events may arrive before the reply and must not be taken for it.
		_, _ = c.Write([]byte(`{"event":"POWERDOWN"}` + "\n" + `{"event":"SHUTDOWN","data":{"guest":true}}` + "\n" + `{"return":{}}` + "\n"))
	})
	require.NoError(t, qmpDo(addr, "quit"))
	assert.JSONEq(t, `{"execute":"quit"}`, got.Load().(string))
}

func TestFaults_QMPSocketThatIsNotThere(t *testing.T) {
	dir, err := os.MkdirTemp("", "qmp")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.Error(t, qmpDo("unix:"+filepath.Join(dir, "missing"), "system_powerdown"))

	// A socket file left by a hypervisor that died: nobody listens.
	stale := filepath.Join(dir, "stale")
	ln, err := net.Listen("unix", stale)
	require.NoError(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())
	require.FileExists(t, stale)
	require.Error(t, qmpDo("unix:"+stale, "system_powerdown"))

	// removeQMPSocket only ever deletes Unix socket paths.
	removeQMPSocket("unix:" + stale)
	assert.NoFileExists(t, stale)
	victim := filepath.Join(dir, "127.0.0.1:4444")
	require.NoError(t, os.WriteFile(victim, nil, 0o600))
	t.Chdir(dir)
	removeQMPSocket("tcp:127.0.0.1:4444")
	removeQMPSocket("127.0.0.1:4444")
	removeQMPSocket("")
	assert.FileExists(t, victim)
}

func TestFaults_RuntimeDirMustBePrivate(t *testing.T) {
	dir := t.TempDir()
	require.Error(t, checkPrivateDir(filepath.Join(dir, "missing")))
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	require.Error(t, checkPrivateDir(file), "a file squatting the runtime directory path was accepted")

	// A directory someone opened up is closed again rather than used as is.
	open := filepath.Join(dir, "open")
	require.NoError(t, os.Mkdir(open, 0o700))
	require.NoError(t, os.Chmod(open, 0o777))
	require.NoError(t, checkPrivateDir(open))
	info, err := os.Stat(open)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	// The directory actually in use passes its own check.
	require.NoError(t, checkPrivateDir(qmpRuntimeDir()))
}

func TestFaults_RuntimeDirFailsClosed(t *testing.T) {
	uid := os.Getuid()
	noTemp := func(string, string) (string, error) { return "", syscall.ENOSPC }

	// The expected path is ours: it is used as is.
	base := t.TempDir()
	dir, err := resolveRuntimeDir(base, uid, noTemp)
	require.NoError(t, err)
	assert.Equal(t, expectedRuntimeDir(base, uid), dir)

	// Someone else put a link at the expected path; a fresh private
	// directory is used instead.
	base = t.TempDir()
	target := t.TempDir()
	require.NoError(t, os.Symlink(target, expectedRuntimeDir(base, uid)))
	dir, err = resolveRuntimeDir(base, uid, os.MkdirTemp)
	require.NoError(t, err)
	assert.NotEqual(t, expectedRuntimeDir(base, uid), dir)
	require.NoError(t, checkPrivateDir(dir))

	// Neither is possible: an error, never the rejected path.
	dir, err = resolveRuntimeDir(base, uid, noTemp)
	require.ErrorIs(t, err, syscall.ENOSPC)
	assert.Empty(t, dir)

	// Nothing could be created at all (the temp dir takes no new entries):
	// the not-yet-existing predictable path, which anyone could create later,
	// is not handed out either.
	missing := filepath.Join(t.TempDir(), "gone")
	dir, err = resolveRuntimeDir(missing, uid, noTemp)
	require.Error(t, err)
	assert.Empty(t, dir)

	// mkdirTemp handing back something that is not private is refused too.
	base = t.TempDir()
	require.NoError(t, os.Symlink(target, expectedRuntimeDir(base, uid)))
	_, err = resolveRuntimeDir(base, uid, func(string, string) (string, error) {
		return expectedRuntimeDir(base, uid), nil
	})
	require.Error(t, err)
}

// --- Guest log ---------------------------------------------------------------

func TestFaults_LogBufferStaysBoundedUnderAFlood(t *testing.T) {
	b := &safeBuffer{max: 1024}
	// A guest stuck in a crash loop prints forever.
	for i := range 5000 {
		n, err := fmt.Fprintf(b, "line %06d\n", i)
		require.NoError(t, err)
		require.Equal(t, 12, n, "a short write would make the hypervisor's output pipe stall")
	}
	got := b.Bytes()
	assert.Len(t, got, 1024)
	assert.True(t, bytes.HasSuffix(got, []byte("line 004999\n")), "the newest output must be what is kept")
	assert.NotContains(t, string(got), "line 000000")

	// One write larger than the whole buffer keeps its tail.
	big := bytes.Repeat([]byte("x"), 5000)
	big = append(big, []byte("THE END")...)
	n, err := b.Write(big)
	require.NoError(t, err)
	assert.Equal(t, len(big), n)
	got = b.Bytes()
	assert.Len(t, got, 1024)
	assert.True(t, bytes.HasSuffix(got, []byte("THE END")))

	// Bytes hands out a copy: a reader cannot corrupt the buffer.
	got[0] = '!'
	assert.NotEqual(t, byte('!'), b.Bytes()[0])

	n, err = b.Write(nil)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestFaults_LogBufferConcurrentWritersAndReaders(t *testing.T) {
	b := &safeBuffer{max: 4096}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			line := []byte(strings.Repeat(fmt.Sprint(w), 63) + "\n")
			for range 500 {
				_, _ = b.Write(line)
				_ = b.Bytes()
			}
		}()
	}
	wg.Wait()
	got := b.Bytes()
	require.Len(t, got, 4096)
	// Writes are atomic: every kept line is one writer's line, never a mix.
	for _, line := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n") {
		require.Len(t, line, 63)
		require.Equal(t, strings.Repeat(line[:1], 63), line)
	}
}

func TestFaults_LogLimitFallsBackToTheDefault(t *testing.T) {
	t.Cleanup(func() { SetVMLogMaxBytes(0) })
	for _, bad := range []int64{0, -1} {
		SetVMLogMaxBytes(bad)
		assert.Equal(t, defaultVMLogMaxBytes, vmLogMaxBytes.Load(), "a limit of %d would disable or break log capture", bad)
	}
	SetVMLogMaxBytes(100)
	b := &safeBuffer{}
	_, _ = b.Write(bytes.Repeat([]byte("a"), 250))
	assert.Len(t, b.Bytes(), 100)
}

// --- Volume mount watch ------------------------------------------------------

func TestFaults_MountFailureInADamagedLog(t *testing.T) {
	cases := map[string]struct {
		log    string
		want   string
		failed bool
	}{
		"clean boot":             {"en1: assigned 10.0.2.15\nstorage: mounted /data\n", "", false},
		"mount point missing":    {"boot\nstorage: mount point /data not found\n", "/data", true},
		"invalid mount point":    {"storage: invalid mount point data\n", "data", true},
		"log cut after the path": {"storage: mount point /data", "/data", true},
		// The log was cut inside the marker or right after it: no path yet.
		"log cut inside the marker": {"storage: mount poi", "", false},
		"log cut before the path":   {"storage: mount point ", "", false},
		"binary noise":              {"\x00\xff\x00storage: mount point /d\x00ata\r\n", "/d\x00ata", true},
		"empty":                     {"", "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mp, failed := detectMountFailure([]byte(tc.log))
			assert.Equal(t, tc.failed, failed)
			assert.Equal(t, tc.want, mp)
		})
	}
}

func TestFaults_MountWatchEndsWithTheVM(t *testing.T) {
	oldInterval, oldTimeout := mountCheckInterval, mountCheckTimeout
	mountCheckInterval, mountCheckTimeout = 5*time.Millisecond, time.Hour
	t.Cleanup(func() { mountCheckInterval, mountCheckTimeout = oldInterval, oldTimeout })

	watch := func(v *VM, ctx context.Context) chan struct{} {
		finished := make(chan struct{})
		go func() { watchVolumeMounts(ctx, v); close(finished) }()
		return finished
	}
	requireFinished := func(finished chan struct{}, why string) {
		t.Helper()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Fatal(why)
		}
	}

	t.Run("failure printed late is reported once", func(t *testing.T) {
		v := &VM{ID: "late", done: make(chan struct{})}
		finished := watch(v, context.Background())
		_, _ = v.logBuf.Write([]byte("booting\n"))
		_, _ = v.logBuf.Write([]byte("storage: mount point /data not found\n"))
		requireFinished(finished, "the watch did not notice the mount failure")
		require.Len(t, v.Warnings(), 1)
		assert.Contains(t, v.Warnings()[0], `"/data"`)
		// A second watch (a restart of the same VM object) adds no duplicate.
		requireFinished(watch(v, context.Background()), "second watch did not finish")
		assert.Len(t, v.Warnings(), 1)
	})
	t.Run("vm dies during boot", func(t *testing.T) {
		v := &VM{ID: "dies", State: StateRunning, done: make(chan struct{})}
		finished := watch(v, context.Background())
		require.NoError(t, v.transition(StateStopped))
		requireFinished(finished, "the watch outlived its VM")
		assert.Empty(t, v.Warnings())
	})
	t.Run("daemon shuts down", func(t *testing.T) {
		v := &VM{ID: "ctx", done: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		finished := watch(v, ctx)
		cancel()
		requireFinished(finished, "the watch ignored its context")
	})
	t.Run("boot window passes", func(t *testing.T) {
		mountCheckTimeout = 30 * time.Millisecond
		v := &VM{ID: "quiet", done: make(chan struct{})}
		requireFinished(watch(v, context.Background()), "the watch never gave up on a quiet guest")
		assert.Empty(t, v.Warnings())
	})
}

// --- Port maps ---------------------------------------------------------------

func TestFaults_PortMapInputThatIsNotAPortMap(t *testing.T) {
	for _, bad := range []string{
		"", ":", "/", "80", "80:", ":80", "a:b", "0:80", "80:0", "65536:80", "80:65536",
		"-1:80", "80:-1", "80:80/sctp", "80:80/", "80:80:80", "8080:80/tcp/udp",
		"0x50:80", "80.0:80", "999999999999999999999:80", "１２:80",
	} {
		pm, err := ParsePortMap(bad)
		require.Error(t, err, "%q", bad)
		assert.Equal(t, PortMap{}, pm, "%q: a rejected mapping returned a partial value", bad)
	}
	for in, want := range map[string]PortMap{
		"1:1":             {HostPort: 1, GuestPort: 1, Protocol: ProtocolTCP},
		"65535:65535/udp": {HostPort: 65535, GuestPort: 65535, Protocol: ProtocolUDP},
		"8080:80/TCP":     {HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP},
	} {
		pm, err := ParsePortMap(in)
		require.NoError(t, err, "%q", in)
		assert.Equal(t, want, pm)
		// What is printed parses back to the same mapping.
		again, err := ParsePortMap(pm.String())
		require.NoError(t, err)
		assert.Equal(t, pm, again)
	}
	// Spaces around the numbers, as a quoted shell argument may carry.
	padded, err := ParsePortMap(" 8080 : 80 ")
	require.NoError(t, err)
	assert.Equal(t, PortMap{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}, padded)

	// One bad mapping fails the whole list: no VM starts with half its ports.
	got, err := ParsePortMaps([]string{"8080:80", "nope", "9090:90"})
	require.Error(t, err)
	assert.Nil(t, got)
	got, err = ParsePortMaps(nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// --- Store lookups -----------------------------------------------------------

func TestFaults_ResolveNeverGuesses(t *testing.T) {
	s := NewMemoryStore()
	put := func(id, name string) *VM {
		v := &VM{ID: id, Cfg: Config{Name: name}, State: StateCreated, done: make(chan struct{})}
		s.vms[id] = v
		return v
	}
	a := put("abc123000000", "web")
	b := put("abc456000000", "db")
	c := put("def000000000", "abc") // its name is a prefix of other IDs

	_, err := s.Resolve("abc")
	require.NoError(t, err)
	got, _ := s.Resolve("abc")
	assert.Same(t, c, got, "an exact name must win over an ID prefix")
	_, err = s.Resolve("ab")
	require.ErrorContains(t, err, "ambiguous", "an ambiguous prefix must not pick a VM to stop or remove")
	got, err = s.Resolve("abc1")
	require.NoError(t, err)
	assert.Same(t, a, got)
	got, err = s.Resolve("db")
	require.NoError(t, err)
	assert.Same(t, b, got)
	_, err = s.Resolve("abc123000000ffff")
	require.Error(t, err, "longer than any ID")
	_, err = s.Resolve("zzz")
	require.Error(t, err)

	require.NoError(t, s.Remove(a.ID))
	got, err = s.Resolve("ab")
	require.NoError(t, err, "with one candidate left the prefix is unambiguous")
	assert.Same(t, b, got)
	require.Error(t, s.Remove(a.ID))
}

func TestFaults_NewVMIDsDoNotCollide(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		id, err := newID()
		require.NoError(t, err)
		require.Len(t, id, 12)
		require.False(t, seen[id], "duplicate VM id %s", id)
		seen[id] = true
	}
}
