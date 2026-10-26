package vm

import (
	"context"
	"errors"
	"os/exec"
)

// VMMExecArg is the re-exec argument used on Linux; macOS has no ambient
// capabilities to drop, so VMMs start directly.
const VMMExecArg = "__jerboa-vmm-exec"

func vmmCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// ExecVMM is only used on Linux.
func ExecVMM([]string) error { return errors.New("vmm exec: not supported on macOS") }
