package hypervisor

const (
	microVM = Spark
	label   = "Spark"
	// legacyMicroVM is the value macOS installations before the rename to
	// Spark used; it is still accepted and never shown.
	legacyMicroVM = Firecracker
)
