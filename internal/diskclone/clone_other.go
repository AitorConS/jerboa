//go:build !linux && !darwin

package diskclone

// cloneFile has no copy-on-write clone primitive on this platform.
func cloneFile(_, _ string) bool { return false }
