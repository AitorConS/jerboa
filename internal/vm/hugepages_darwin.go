//go:build darwin && arm64

package vm

// fcHugePages: the macOS VMM has no huge_pages setting (and rejects unknown
// fields), so it is never set.
func fcHugePages(int) string { return "" }
