//go:build linux || (darwin && arm64)

package vm

import (
	"errors"
	"os/exec"
	"strings"
)

// guestExitCode extracts the unikernel guest's own exit code from the hypervisor
// process's wait result.
//
// QEMU is launched with `-device isa-debug-exit,iobase=0x501,iosize=1`. Nanos
// writes the process exit status to port 0x501 on shutdown (QEMU_HALT), so QEMU
// terminates with process status (guestCode<<1)|1 — always odd. Reversing that
// encoding recovers the guest code: a clean guest exit(0) yields process status
// 1 → code 0; a crash exit(3) yields status 7 → code 3.
//
// ok is false when the status does not carry a guest code: the process was
// signaled (e.g. SIGKILL), exited 0 (no debug-exit write — a Firecracker guest,
// which has no such device, or an externally reset VM), or exited with an even
// status. Callers fall back to the raw hypervisor result in those cases.
//
// Note the encoding is ambiguous for process status 1: it decodes to guest
// code 0, but QEMU also exits 1 on its OWN errors (bad -drive, no KVM, …). Since
// isa-debug-exit calls exit() directly with no QMP SHUTDOWN event, the two are
// indistinguishable by status alone; isFailureExit breaks the tie on stderr.
func guestExitCode(waitErr error) (code int, ok bool) {
	if waitErr == nil {
		return 0, false
	}
	var ee *exec.ExitError
	if !errors.As(waitErr, &ee) {
		return 0, false
	}
	status := ee.ExitCode()
	if status <= 0 { // -1 == signaled; 0 == clean process exit with no debug-exit write
		return 0, false
	}
	if status&1 == 0 { // even status: not an isa-debug-exit encoding
		return 0, false
	}
	return status >> 1, true
}

// isFailureExit reports whether a QEMU VM's exit should be treated as a failure
// for the `on-failure` restart policy. qemuStderr is QEMU's own captured stderr
// (see VM.qemuErrBuf), consulted only to break the status-1 tie below.
//
// When the guest exit code is recoverable (QEMU + isa-debug-exit) and non-zero,
// it is a crash and restarts. This is the whole point of the isa-debug-exit
// wiring — previously a guest crash powered the VM off and left the hypervisor
// exiting 0, so on-failure never fired.
//
// A recovered guest code of 0 is the ambiguous case: it means process status 1,
// which is BOTH a clean guest exit(0) AND QEMU's own exit(1) on a startup or
// runtime error, because isa-debug-exit calls exit((code<<1)|1) directly with no
// QMP SHUTDOWN event. QEMU prints a diagnostic to stderr when it fails on its
// own; a genuine guest exit(0) leaves stderr clean. The tie is broken on that:
// an error on QEMU's stderr means a QEMU failure (restart), a clean stderr means
// a clean guest shutdown (do not restart).
//
// When the guest code cannot be recovered (a signaled process, or an even
// status), it falls back to the raw hypervisor result: any abnormal (non-nil)
// exit is a failure.
func isFailureExit(waitErr error, qemuStderr string) bool {
	code, ok := guestExitCode(waitErr)
	if !ok {
		return waitErr != nil
	}
	if code != 0 {
		return true // guest crashed: exit(N>0)
	}
	// code == 0 → process status 1: clean guest exit(0) vs QEMU's own exit(1).
	return qemuErrored(qemuStderr)
}

// qemuErrored reports whether QEMU's captured stderr shows QEMU failing on its
// own rather than running cleanly. QEMU prints fatal errors as
// "<progname>: <message>" and non-fatal notes as "<progname>: warning: …"; the
// guest's serial console is captured separately (on stdout), so anything here is
// QEMU's own output. Only a non-warning qemu diagnostic line counts, so a stray
// warning does not force a spurious restart of a cleanly-exited guest.
func qemuErrored(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		// QEMU tags its own diagnostics with the "qemu…:" program-name prefix
		// (e.g. "qemu-system-x86_64: could not open disk image").
		if !strings.Contains(lower, "qemu") || !strings.Contains(line, ":") {
			continue
		}
		if strings.Contains(lower, "warning:") {
			continue
		}
		return true
	}
	return false
}
