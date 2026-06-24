//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKernelCheck_OutputsInstalledVersion(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "kernel", "check")
	require.Contains(t, out, "Installed kernel:")
}

func TestKernelCheck_NoNetwork(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "kernel", "check")
	require.Contains(t, out, "Installed kernel:")
}

func TestConfirmPrompt(t *testing.T) {
	orig := os.Stdin
	t.Cleanup(func() { os.Stdin = orig })

	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString("yes\n")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	os.Stdin = r

	require.True(t, confirmPrompt("Proceed? "))
}

func TestKernelUse_AlreadyOnVersion(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	home := t.TempDir()
	setHomeForTest(t, home)

	toolsDir := filepath.Join(home, ".uni", "tools")
	require.NoError(t, os.MkdirAll(toolsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(toolsDir, "kernel-version.txt"), []byte("v1.2.3\n"), 0o644))

	out := execRoot(t, socketPath, storePath, "kernel", "use", "1.2.3")
	require.Contains(t, out, "Already on kernel v1.2.3")
}

func setHomeForTest(t *testing.T, home string) {
	t.Helper()
	keys := []string{"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH"}
	old := map[string]string{}
	for _, k := range keys {
		old[k] = os.Getenv(k)
	}
	require.NoError(t, os.Setenv("HOME", home))
	require.NoError(t, os.Setenv("USERPROFILE", home))
	if len(home) >= 2 && home[1] == ':' {
		require.NoError(t, os.Setenv("HOMEDRIVE", home[:2]))
		rest := strings.TrimPrefix(home[2:], "\\")
		rest = strings.TrimPrefix(rest, "/")
		require.NoError(t, os.Setenv("HOMEPATH", "\\"+rest))
	}
	t.Cleanup(func() {
		for _, k := range keys {
			_ = os.Setenv(k, old[k])
		}
	})
}
