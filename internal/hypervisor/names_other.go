//go:build !darwin

package hypervisor

const (
	microVM       = Firecracker
	label         = "Firecracker"
	legacyMicroVM = Firecracker
)
