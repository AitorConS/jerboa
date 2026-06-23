//go:build windows

package vm

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// platformInitFC overrides FirecrackerManager hooks to run Firecracker inside
// WSL2, since Firecracker requires Linux/KVM which is not available natively
// on Windows.
func platformInitFC(m *FirecrackerManager) {
	m.mkCmd = wslFCCommandFunc
	m.vmSockPath = wslFCSockPath
	m.cfgPathForProcess = wslWindowsToWSLPath
	m.shutdownAPI = wslFCSendCtrlAltDel
	m.rewriteConfigPaths = wslRewriteFCConfigPaths
	m.vmmLogPath = wslFCVMMLogPath
	m.readVMMLog = wslReadFile
}

func wslFCVMMLogPath(id string) string {
	return "/tmp/fc-" + id + "-vmm.log"
}

func wslReadFile(path string) ([]byte, error) {
	return exec.Command("wsl", "--", "cat", path).Output()
}

// wslLinuxPath is a safe, Windows-free PATH for non-interactive WSL sessions.
// We cannot use $PATH because WSL appends Windows paths (e.g. /mnt/c/Program Files/...)
// which contain spaces and break PATH assignments in bash.
const wslLinuxPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// wslFCCommandFunc runs firecracker inside WSL2 via "bash -c 'exec ...'".
// Using exec replaces bash with the firecracker process so cmd.Process maps
// directly to the VM. A fixed Linux-only PATH avoids WSL's Windows path
// injection breaking the assignment, while $HOME/.local/bin covers
// user-local installs not included in non-interactive sessions.
func wslFCCommandFunc(ctx context.Context, name string, args ...string) *exec.Cmd {
	all := append([]string{name}, args...)
	quoted := make([]string, len(all))
	for i, a := range all {
		quoted[i] = wslShellQuote(a)
	}
	shellCmd := "PATH=$HOME/.local/bin:" + wslLinuxPath + " exec " + strings.Join(quoted, " ")
	return exec.CommandContext(ctx, "wsl", "--", "bash", "-c", shellCmd)
}

// wslShellQuote single-quotes a string for safe use as a bash argument.
func wslShellQuote(s string) string {
	if !strings.ContainsAny(s, " \t\n\"'\\$`!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// wslFCSockPath returns the socket path inside WSL2's /tmp, which is where
// the firecracker process creates its API socket.
func wslFCSockPath(id string) string {
	return "/tmp/fc-" + id + ".sock"
}

// wslFCSendCtrlAltDel sends the Firecracker shutdown action via WSL2's curl,
// since the socket lives inside WSL2 and is not reachable via Windows AF_UNIX.
func wslFCSendCtrlAltDel(sockPath string) error {
	body := `{"action_type":"SendCtrlAltDel"}`
	cmd := exec.Command("wsl", "--", "curl", "-s", "-f", "-X", "PUT",
		"--unix-socket", sockPath,
		"http://localhost/actions",
		"-H", "Content-Type: application/json",
		"-d", body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wsl SendCtrlAltDel: %w: %s", err, out)
	}
	return nil
}

// wslRewriteFCConfigPaths converts Windows absolute paths inside the
// Firecracker JSON config to WSL2 mount paths (/mnt/c/...) so that
// the firecracker process running inside WSL2 can access them.
func wslRewriteFCConfigPaths(cfg *fcVMConfig) {
	cfg.BootSource.KernelImagePath = wslWindowsToWSLPath(cfg.BootSource.KernelImagePath)
	for i := range cfg.Drives {
		cfg.Drives[i].PathOnHost = wslWindowsToWSLPath(cfg.Drives[i].PathOnHost)
	}
}

// wslWindowsToWSLPath converts a Windows absolute path to its WSL2 mount path.
// Example: C:\Users\foo\bar → /mnt/c/Users/foo/bar
func wslWindowsToWSLPath(p string) string {
	p = filepath.ToSlash(p)
	if len(p) >= 2 && p[1] == ':' {
		drive := strings.ToLower(string(p[0]))
		rest := p[2:]
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}
		return "/mnt/" + drive + rest
	}
	return p
}
