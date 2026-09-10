package vm

import (
	"context"
	"fmt"
	"os/exec"
)

func validateHostConfig(cfg Config, _ string) error {
	if cfg.EmulateX86 {
		return fmt.Errorf("--emulate-x86 is a macOS compatibility option; Linux selects its accelerator directly")
	}
	return nil
}
func (m *QEMUManager) buildNativeCmd(ctx context.Context, cfg Config, qmp string) *exec.Cmd {
	panic("macOS backend on Linux")
}
func DefaultQEMUBinary() string { return "qemu-system-x86_64" }

func (m *QEMUManager) prepareHostVM(*VM) (func(), error)        { return nil, nil }
func (m *QEMUManager) RestoreHostRuntime(context.Context) error { return nil }

// Native networking is implemented only on macOS.
type nativeNetworkState struct{}
