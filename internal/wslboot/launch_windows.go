//go:build windows

package wslboot

import "syscall"

// Windows process creation flags for fully detaching the launched daemon from
// the client's console so it survives the client exiting.
const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

// detachAttr detaches the `wsl` launcher from the client console so the daemon
// keeps running after the client process exits.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess | createNoWindow,
	}
}
