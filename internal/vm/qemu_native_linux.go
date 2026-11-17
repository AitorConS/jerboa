package vm

import (
	"context"
	"fmt"
	"os/exec"
)

// Linux guests use TAP networking: the nftables guard keeps them off the
// host's loopback.
func (m *QEMUManager) initGuestEgress() {}

func validateHostConfig(cfg Config, _ string) error {
	if cfg.EmulateX86 {
		return fmt.Errorf("--emulate-x86 is a macOS compatibility option; Linux selects its accelerator directly")
	}
	return validateDiskIOHost(cfg)
}
func (m *QEMUManager) buildNativeCmd(ctx context.Context, cfg Config, qmp string) *exec.Cmd {
	panic("macOS backend on Linux")
}
func DefaultQEMUBinary() string { return "qemu-system-x86_64" }

func (m *QEMUManager) prepareHostVM(*VM) (func(), error) { return nil, nil }

// RestoreHostRuntime gives the VMs re-adopted from the previous daemon back
// what lived in that daemon's process: see restoreAdopted.
func (m *QEMUManager) RestoreHostRuntime(ctx context.Context) error {
	restoreAdopted(ctx, m.store.List(), m.hchecker, func(id string) []string {
		return []string{m.bootDiskPath(id)}
	})
	return nil
}
