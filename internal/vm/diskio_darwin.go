//go:build darwin && arm64

package vm

// defaultFCIOEngine: the macOS VMM runs block I/O on its own worker pool;
// Sync is the only engine it accepts.
func defaultFCIOEngine() string { return fcEngineSync }
