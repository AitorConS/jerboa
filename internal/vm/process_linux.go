package vm

import (
	"fmt"
	"os"
)

// processOwnsVM reports whether the live process pid is the VMM of vmID: the
// QEMU started with the VM's QMP socket, or the Firecracker started with its
// API socket. Both paths are named after the VM, so a process that took the
// PID of a VMM that died does not match.
func processOwnsVM(pid int, vmID string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return cmdlineOwnsVM(data, qmpSocketPath(vmID), fcSocketPath(vmID))
}
