// Package wsldistro manages the dedicated jerboa WSL2 distribution: a versioned,
// self-contained Linux environment (jerboad + qemu + firecracker + kernel
// toolchain) imported via `wsl --import`, the way Docker Desktop ships its own
// distro. Running the daemon inside it removes any dependency on the user's WSL
// setup, on jerboad being on PATH, or on host sudo.
package wsldistro

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Name is the registered WSL2 distribution name.
const Name = "jerboa"

// RootfsArtifact is the release asset holding the distro root filesystem.
const RootfsArtifact = "jerboa-rootfs-amd64.tar.gz"

// DefaultInstallDir returns where the distro's ext4 disk is stored on Windows
// (%LOCALAPPDATA%\jerboa\distro), falling back to ~/.jerboa/distro.
func DefaultInstallDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "jerboa", "distro")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".jerboa", "distro")
	}
	return filepath.Join(home, ".jerboa", "distro")
}

// Exists reports whether the jerboa distro is registered with WSL.
func Exists() (bool, error) {
	names, err := List()
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if strings.EqualFold(n, Name) {
			return true, nil
		}
	}
	return false, nil
}

// List returns the names of all registered WSL2 distributions.
func List() ([]string, error) {
	out, err := exec.Command("wsl", "--list", "--quiet").CombinedOutput() //nolint:gosec,noctx // fixed program, no args
	if err != nil {
		return nil, fmt.Errorf("wsldistro: list distros: %w (%s)", err, strings.TrimSpace(decodeWSLOutput(out)))
	}
	return parseDistroList(out), nil
}

// Import registers the distro from rootfsTar, storing its disk under installDir.
func Import(installDir, rootfsTar string) error {
	if _, err := os.Stat(rootfsTar); err != nil {
		return fmt.Errorf("wsldistro: rootfs %q: %w", rootfsTar, err)
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return fmt.Errorf("wsldistro: create install dir: %w", err)
	}
	out, err := exec.Command("wsl", "--import", Name, installDir, rootfsTar, "--version", "2").CombinedOutput() //nolint:gosec,noctx // controlled args
	if err != nil {
		return fmt.Errorf("wsldistro: import %s: %w (%s)", Name, err, strings.TrimSpace(decodeWSLOutput(out)))
	}
	return nil
}

// IP returns the IPv4 address of the running distro's primary interface. WSL2
// loopback forwarding does not reach a secondary distro, so the Windows client
// dials this address directly. Querying it starts the distro if it is stopped.
func IP() (string, error) {
	out, err := exec.Command("wsl", "-d", Name, "-u", "root", "--", "hostname", "-I").CombinedOutput() //nolint:gosec,noctx // fixed args
	if err != nil {
		return "", fmt.Errorf("wsldistro: distro ip: %w (%s)", err, strings.TrimSpace(decodeWSLOutput(out)))
	}
	fields := strings.Fields(decodeWSLOutput(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("wsldistro: %s has no IP yet", Name)
	}
	return fields[0], nil
}

// Unregister removes the distro and all of its data.
func Unregister() error {
	out, err := exec.Command("wsl", "--unregister", Name).CombinedOutput() //nolint:gosec,noctx // fixed args
	if err != nil {
		return fmt.Errorf("wsldistro: unregister %s: %w (%s)", Name, err, strings.TrimSpace(decodeWSLOutput(out)))
	}
	return nil
}

// parseDistroList extracts distro names from `wsl --list --quiet` output.
func parseDistroList(raw []byte) []string {
	var names []string
	for line := range strings.SplitSeq(decodeWSLOutput(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

// decodeWSLOutput turns wsl.exe's UTF-16LE output into plain text. WSL emits
// UTF-16; dropping the NUL bytes recovers the ASCII distro names and messages.
func decodeWSLOutput(b []byte) string {
	return string(bytes.ReplaceAll(b, []byte{0}, nil))
}
