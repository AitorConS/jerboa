//go:build !linux && !darwin

package hypervisor

// unsupported: the daemon runs VMs on Linux and macOS only.
func unsupported() string { return "Firecracker runs on Linux and macOS" }
