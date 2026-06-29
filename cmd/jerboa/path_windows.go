//go:build windows

package main

import "strings"

// hostPathForDaemon translates a Windows absolute path to its WSL2 equivalent
// so the Linux daemon can open the file via the /mnt/<drive>/... mount point.
// Example: C:\Users\foo\bar → /mnt/c/Users/foo/bar
func hostPathForDaemon(p string) string {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		drive := strings.ToLower(string(p[0]))
		rest := strings.ReplaceAll(p[3:], "\\", "/")
		return "/mnt/" + drive + "/" + rest
	}
	return strings.ReplaceAll(p, "\\", "/")
}
