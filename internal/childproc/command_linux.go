package childproc

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

// ReexecArg is the first argument with which a Jerboa binary re-executes
// itself to start a program without its ambient capabilities: HandleReexec
// then clears them and execs the program in the same process.
//
// The systemd unit gives the daemon CAP_NET_ADMIN, CAP_NET_RAW and
// CAP_NET_BIND_SERVICE as ambient capabilities, which are kept across execve.
// QEMU, Firecracker and mkfs need none of them, so a guest that took over its
// VMM, or a package that took over mkfs, must not get them either. Go's exec
// cannot clear ambient capabilities in the child before execve, hence the
// re-exec.
const ReexecArg = "__jerboa-vmm-exec"

// ambientCapsFile is read to tell whether this process holds ambient capabilities.
var ambientCapsFile = "/proc/self/status"

// Command is exec.CommandContext for a program that handles untrusted input
// (a VMM, mkfs): its environment is Minimal, and it is routed through
// HandleReexec when this process holds ambient capabilities. Without any (a
// daemon run by root, the CLI, a test) there is nothing to drop and the
// program is started directly. Callers may append to cmd.Env.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := command(ctx, name, args...)
	cmd.Env = Minimal()
	return cmd
}

func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if !hasAmbientCaps() {
		return exec.CommandContext(ctx, name, args...)
	}
	self, err := os.Executable()
	if err != nil {
		return exec.CommandContext(ctx, name, args...)
	}
	return exec.CommandContext(ctx, self, append([]string{ReexecArg, name}, args...)...)
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

// HandleReexec runs first in main of every binary that can start a program
// through Command. When the process was re-executed with ReexecArg it clears
// the ambient capabilities and replaces itself with the program, with the
// environment Command gave it; it returns only when it was not.
func HandleReexec() {
	if len(os.Args) < 2 || os.Args[1] != ReexecArg {
		return
	}
	err := Exec(os.Args[2:])
	fmt.Fprintln(os.Stderr, err)
	os.Exit(127)
}

// Exec clears the ambient capabilities and replaces the process with the
// program named by args[0]. It returns only on failure.
func Exec(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("reexec: no program")
	}
	// Capabilities are per thread, and execve runs on this one.
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("reexec: clear ambient capabilities: %w", err)
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("reexec: %w", err)
	}
	// os.Environ is the environment Command chose for the program.
	if err := syscall.Exec(path, args, os.Environ()); err != nil { //nolint:gosec // a program and arguments chosen by this binary
		return fmt.Errorf("reexec %s: %w", path, err)
	}
	return nil
}

// SetStatusFileForTest makes Command read the ambient capabilities from path,
// a file in the format of /proc/self/status, and returns what restores the
// default. It lets the tests of packages that start programs through Command
// check that they take the re-exec path; a Command built that way must not be
// started from a test binary, which does not call HandleReexec.
func SetStatusFileForTest(path string) (restore func()) {
	prev := ambientCapsFile
	ambientCapsFile = path
	return func() { ambientCapsFile = prev }
}
