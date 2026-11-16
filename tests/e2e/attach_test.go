//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// syncBuffer collects the output of a process another goroutine reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRunAttachShowsConsoleAndReturns runs a guest that prints and exits
// with `jerboa run --attach --rm`: the command must print the guest's
// output, return when the guest ends, and leave no VM behind.
func TestRunAttachShowsConsoleAndReturns(t *testing.T) {
	eachHypervisor(t, func(t *testing.T, hv string) {
		e := newEnv(t, withHypervisor(hv))
		e.buildProbe()
		before := len(e.ps())

		res := e.runFor(2*time.Minute, "run", "probe", "--attach", "--rm", "-e", "PROBE_EXIT=0")
		require.Zerof(t, res.code, "run --attach of a guest that exits 0:\n%s", res.all())
		require.Contains(t, res.stdout, "PROBE_EXIT 0", "run --attach must print the guest's console")

		require.Eventually(t, func() bool { return len(e.ps()) == before }, 30*time.Second, 200*time.Millisecond,
			"--rm must remove the VM once the attached guest has ended")
	})
}

// attachedFlood starts `jerboa run --attach --rm` on a guest that writes to
// its console without pause, and returns the CLI process and the VM once
// the output is flowing.
func attachedFlood(t *testing.T, e *env) (pid int, wait func(), vmID string, out *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := e.command(ctx, "run", "probe", "--attach", "--rm", "--name", "flood", "-e", "PROBE_FLOOD=1")
	out = &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	wait = func() {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("the attached CLI did not exit")
		}
	}
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "PROBE_FLOOD 100 ") }, 90*time.Second, 100*time.Millisecond,
		"the attached console never showed the guest's output")
	for _, v := range e.ps() {
		if v.Name == "flood" {
			vmID = v.ID
		}
	}
	require.NotEmpty(t, vmID, "the attached VM is not listed")
	e.vmIDs[vmID] = true
	require.Equal(t, "running", e.vmState(vmID))
	return cmd.Process.Pid, wait, vmID, out
}

// waitReaped waits for what must follow the end of an attached --rm guest
// whatever its reader does: the VM is stopped at once and its VMM process is
// gone, and the VM is removed once the daemon gives up on a reader that
// does not take the end of the stream (30 seconds).
func waitReaped(e *env, vmID string) {
	e.t.Helper()
	require.Eventuallyf(e.t, func() bool { st := e.vmState(vmID); return st == "stopped" || st == "" },
		20*time.Second, 200*time.Millisecond, "VM %s was not reaped: its state is still %q", vmID, e.vmState(vmID))
	e.waitNoVMM(vmID, 15*time.Second)
	e.waitGone(vmID, 60*time.Second)
}

// TestAttachedVMIsReapedWhenNobodyReads covers the console nobody reads any
// more. A guest floods its console under `run --attach --rm`; then the
// reader goes away: once killed outright, once frozen (a terminal in scroll
// lock, a pipe to a stuck program). In both cases `jerboa stop` must stop
// the VM, the VMM process must be gone, and --rm must remove it: a console
// without a reader must not hold the VM.
func TestAttachedVMIsReapedWhenNobodyReads(t *testing.T) {
	eachHypervisor(t, func(t *testing.T, hv string) {
		for _, tc := range []struct {
			name string
			sig  syscall.Signal
		}{
			{"reader killed", syscall.SIGKILL},
			{"reader frozen", syscall.SIGSTOP},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := newEnv(t, withHypervisor(hv))
				e.buildProbe()
				pid, wait, vmID, _ := attachedFlood(t, e)

				require.NoError(t, syscall.Kill(pid, tc.sig))
				if tc.sig == syscall.SIGSTOP {
					t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				}
				// The guest goes on writing with nobody taking its output:
				// far more than any buffer between the VMM and the reader
				// holds.
				time.Sleep(3 * time.Second)
				require.Equal(t, "running", e.vmState(vmID), "losing its reader must not stop the guest")

				res := e.runFor(60*time.Second, "stop", vmID)
				require.Zerof(t, res.code, "stop of a VM whose console nobody reads:\n%s", res.all())
				waitReaped(e, vmID)
				if tc.sig == syscall.SIGSTOP {
					require.NoError(t, syscall.Kill(pid, syscall.SIGKILL))
				}
				wait()

				// The daemon is not left wedged: it runs the next guest.
				res = e.runFor(2*time.Minute, "run", "probe", "--attach", "--rm", "-e", "PROBE_EXIT=0")
				require.Zerof(t, res.code, "run after the reaped VM:\n%s", res.all())
				require.Contains(t, res.stdout, "PROBE_EXIT 0")
			})
		}
	})
}

// TestAttachedGuestThatExitsWhileFlooding checks the other end of the same
// pipe: the guest ends by itself while its attached reader is frozen. The
// VM must still be reaped and removed.
func TestAttachedGuestThatExitsWhileFlooding(t *testing.T) {
	eachHypervisor(t, func(t *testing.T, hv string) {
		e := newEnv(t, withHypervisor(hv))
		e.buildProbe()
		pid, wait, vmID, _ := attachedFlood(t, e)
		require.NoError(t, syscall.Kill(pid, syscall.SIGSTOP))
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		time.Sleep(2 * time.Second)

		// The VMM dies under the guest, as on a crash.
		require.NoError(t, syscall.Kill(e.vmmPID(vmID), syscall.SIGKILL))
		waitReaped(e, vmID)
		require.NoError(t, syscall.Kill(pid, syscall.SIGKILL))
		wait()
		require.Empty(t, e.ps(), "the VM must be gone")
	})
}
