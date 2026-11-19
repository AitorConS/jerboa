// Package childproc decides what the processes Jerboa starts inherit: which
// environment variables (no credentials, and only an allowlist for programs
// that read untrusted input) and, on Linux, which capabilities.
package childproc

import (
	"os"
	"strings"
)

// credentialEnv lists credentials that are not caught by the JERBOA_ pattern
// of IsCredential: what lets a GitHub Actions job request OIDC tokens, which
// push as the job's trusted publisher.
var credentialEnv = map[string]bool{
	"ACTIONS_ID_TOKEN_REQUEST_TOKEN": true,
	"ACTIONS_ID_TOKEN_REQUEST_URL":   true,
	"JERBOA_MINISIGN_SEED":           true,
}

// IsCredential reports whether the environment variable name holds one of
// Jerboa's credentials: any JERBOA_ variable with TOKEN or SECRET in its name
// (the daemon, cluster, observability, agent and registry tokens), the
// release signing seed, and GitHub Actions' OIDC request token.
func IsCredential(name string) bool {
	n := strings.ToUpper(name)
	if credentialEnv[n] {
		return true
	}
	return strings.HasPrefix(n, "JERBOA_") && (strings.Contains(n, "TOKEN") || strings.Contains(n, "SECRET"))
}

// Inherited returns this process's environment without Jerboa's credentials,
// followed by extra. It is the environment of a child that needs the rest of
// the environment (a build step, a host networking tool).
func Inherited(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !IsCredential(name) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// minimalEnv is what a VMM or mkfs gets from this process's environment: how
// to find programs and libraries' data, where home and temporary files are
// (QEMU puts its snapshot=on overlay in TMPDIR), and the locale. The Windows
// names are what a Windows process needs to start at all.
var minimalEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
	"TMPDIR": true, "TMP": true, "TEMP": true,
	"LANG": true, "LANGUAGE": true, "TZ": true,
	"SYSTEMROOT": true, "WINDIR": true, "SYSTEMDRIVE": true, "COMSPEC": true,
	"PATHEXT": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
	"PROGRAMDATA": true,
}

// Minimal returns the allowlisted part of this process's environment (see
// minimalEnv, plus LC_*), followed by extra. It is the environment of a
// program that handles untrusted input, a VMM running a guest or mkfs
// reading a package: it gets nothing it does not need, credentials included.
func Minimal(extra ...string) []string {
	env := make([]string, 0, len(minimalEnv)+len(extra))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		n := strings.ToUpper(name)
		if minimalEnv[n] || strings.HasPrefix(n, "LC_") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}
