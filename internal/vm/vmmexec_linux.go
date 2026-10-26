package vm

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// VMMExecArg is the first argument with which jerboad re-executes itself to
// launch a VMM: ExecVMM then clears the ambient capabilities and execs the
// VMM in the same process.
//
// The systemd unit gives the daemon CAP_NET_ADMIN, CAP_NET_RAW and
// CAP_NET_BIND_SERVICE as ambient capabilities, which are kept across execve.
// QEMU and Firecracker need none of them, so a guest that took over its VMM
// must not get them either. Go's exec cannot clear ambient capabilities in
// the child before execve, hence the re-exec.
const VMMExecArg = "__jerboa-vmm-exec"

// ambientCapsFile is read to tell whether the daemon holds ambient capabilities.
var ambientCapsFile = "/proc/self/status"

// vmmCommand is exec.CommandContext, routed through ExecVMM when the daemon
// holds ambient capabilities. Without any (a daemon run by root, or in a
// test) there is nothing to drop and the VMM is started directly.
func vmmCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if !hasAmbientCaps() {
		return exec.CommandContext(ctx, name, args...)
	}
	self, err := os.Executable()
	if err != nil {
		return exec.CommandContext(ctx, name, args...)
	}
	return exec.CommandContext(ctx, self, append([]string{VMMExecArg, name}, args...)...)
}

func hasAmbientCaps() bool {
	f, err := os.Open(ambientCapsFile)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "CapAmb:"); ok {
			return strings.Trim(strings.TrimSpace(v), "0") != ""
		}
	}
	return false
}

// ExecVMM clears the ambient capabilities and replaces the process with the
// VMM named by args[0]. It returns only on failure.
func ExecVMM(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("vmm exec: no program")
	}
	// Capabilities are per thread, and execve runs on this one.
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("vmm exec: clear ambient capabilities: %w", err)
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("vmm exec: %w", err)
	}
	if err := syscall.Exec(path, args, os.Environ()); err != nil { //nolint:gosec // the daemon's own VMM binary and arguments
		return fmt.Errorf("vmm exec %s: %w", path, err)
	}
	return nil
}
