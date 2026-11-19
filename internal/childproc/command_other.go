//go:build !linux

package childproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// ReexecArg is the re-exec argument used on Linux; other systems have no
// ambient capabilities to drop, so programs start directly.
const ReexecArg = "__jerboa-vmm-exec"

// Command is exec.CommandContext for a program that handles untrusted input
// (a VMM, mkfs), with a Minimal environment. Callers may append to cmd.Env.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = Minimal()
	return cmd
}

// HandleReexec is a no-op unless the process was started with ReexecArg,
// which only happens on Linux.
func HandleReexec() {
	if len(os.Args) < 2 || os.Args[1] != ReexecArg {
		return
	}
	fmt.Fprintln(os.Stderr, Exec(os.Args[2:]))
	os.Exit(127)
}

// Exec is only used on Linux.
func Exec([]string) error { return errors.New("reexec: only supported on Linux") }
