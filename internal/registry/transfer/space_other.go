//go:build !linux && !darwin

package transfer

// statfsSpace cannot tell the free space here; the pull goes ahead without
// the check.
func statfsSpace(string) (int64, uint64, bool) { return 0, 0, false }
