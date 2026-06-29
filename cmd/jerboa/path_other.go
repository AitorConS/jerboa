//go:build !windows

package main

// hostPathForDaemon is a no-op on non-Windows platforms: the daemon runs on
// the same OS as the CLI, so no path translation is needed.
func hostPathForDaemon(p string) string { return p }
