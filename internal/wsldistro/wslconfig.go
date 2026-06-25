package wsldistro

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// nestedVirtKey is the .wslconfig setting that exposes hardware virtualization
// (VT-x/AMD-V) to the WSL2 utility VM. Without it the WSL2 kernel cannot create
// /dev/kvm, so firecracker fails with "Error creating KVM object".
const nestedVirtKey = "nestedVirtualization"

// bom is the UTF-8 byte-order mark some Windows editors (e.g. Notepad) prepend.
const bom = "\uFEFF"

// WSLConfigPath returns the path to the host's per-user .wslconfig
// (%USERPROFILE%\.wslconfig).
func WSLConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("wsldistro: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".wslconfig"), nil
}

// EnsureNestedVirtualization makes sure %USERPROFILE%\.wslconfig enables nested
// virtualization under the [wsl2] section, which firecracker needs for /dev/kvm.
// It edits the file in place, preserving any other settings, and returns whether
// it changed anything. A change only takes effect after `wsl --shutdown`, so the
// caller should tell the user to restart WSL when changed is true.
func EnsureNestedVirtualization() (changed bool, path string, err error) {
	path, err = WSLConfigPath()
	if err != nil {
		return false, "", err
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		content := "[wsl2]\n" + nestedVirtKey + "=true\n"
		if werr := os.WriteFile(path, []byte(content), 0o600); werr != nil {
			return false, path, fmt.Errorf("wsldistro: write %s: %w", path, werr)
		}
		return true, path, nil
	}
	if err != nil {
		return false, path, fmt.Errorf("wsldistro: read %s: %w", path, err)
	}

	updated, changed := setNestedVirtualization(string(data))
	if !changed {
		return false, path, nil
	}
	if werr := os.WriteFile(path, []byte(updated), 0o600); werr != nil {
		return false, path, fmt.Errorf("wsldistro: write %s: %w", path, werr)
	}
	return true, path, nil
}

// setNestedVirtualization returns content with nestedVirtualization=true ensured
// under [wsl2], and whether it had to change anything. It preserves unrelated
// lines, sections, and comments. Exposed (unexported) for testing.
func setNestedVirtualization(content string) (string, bool) {
	// Strip a leading UTF-8 BOM so the first [wsl2] header is recognized; editors
	// like Notepad add one. Writing back without it is harmless — WSL reads either.
	// Keep the original so a no-op returns the file's content untouched.
	original := content
	content = strings.TrimPrefix(content, bom)
	lines := strings.Split(content, "\n")
	inWSL2 := false
	wsl2Start := -1 // index of the [wsl2] header line, -1 until found

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inWSL2 {
				break // left the [wsl2] section without finding the key
			}
			if strings.EqualFold(trimmed, "[wsl2]") {
				inWSL2 = true
				wsl2Start = i
			}
			continue
		}
		if inWSL2 && isNestedVirtKey(trimmed) {
			if nestedVirtTrue(trimmed) {
				return original, false
			}
			lines[i] = nestedVirtKey + "=true"
			return strings.Join(lines, "\n"), true
		}
	}

	// [wsl2] exists but has no nestedVirtualization key: insert it right after
	// the section header.
	if wsl2Start >= 0 {
		insertAt := wsl2Start + 1
		out := append([]string{}, lines[:insertAt]...)
		out = append(out, nestedVirtKey+"=true")
		out = append(out, lines[insertAt:]...)
		return strings.Join(out, "\n"), true
	}

	// No [wsl2] section at all: append one.
	prefix := content
	if prefix != "" && !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	return prefix + "[wsl2]\n" + nestedVirtKey + "=true\n", true
}

// isNestedVirtKey reports whether an .wslconfig line assigns nestedVirtualization
// (case-insensitive), ignoring surrounding whitespace.
func isNestedVirtKey(line string) bool {
	key, _, ok := strings.Cut(line, "=")
	return ok && strings.EqualFold(strings.TrimSpace(key), nestedVirtKey)
}

// nestedVirtTrue reports whether a nestedVirtualization assignment is already true.
func nestedVirtTrue(line string) bool {
	_, val, _ := strings.Cut(line, "=")
	return strings.EqualFold(strings.TrimSpace(val), "true")
}
