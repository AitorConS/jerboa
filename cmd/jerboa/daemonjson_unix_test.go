//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/wslboot"
	"github.com/stretchr/testify/require"
)

// Without a home directory the daemon secret file must go to the per-user
// temp fallback, checked like the config, never to ./.jerboa.
func TestDaemonJSONPath_NoHomeUsesPrivateFallback(t *testing.T) {
	tmp := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv("TMPDIR", tmp)
	t.Chdir(cwd)

	userDir := filepath.Join(tmp, fmt.Sprintf("jerboa-%d", os.Getuid()))
	path := daemonJSONPath()
	require.Equal(t, filepath.Join(userDir, ".jerboa", "daemon.json"), path)

	token, err := wslboot.LoadOrCreateToken(path)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	_, err = os.Lstat(filepath.Join(cwd, ".jerboa"))
	require.True(t, os.IsNotExist(err), "nothing is written to the working directory")
	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	// A fallback directory that is a symlink is refused, not written through.
	require.NoError(t, os.RemoveAll(userDir))
	elsewhere := t.TempDir()
	require.NoError(t, os.Symlink(elsewhere, userDir))
	require.Error(t, wslboot.SaveDaemonFile(path, "secret", "tcp://127.0.0.1:7890"))
	entries, err := os.ReadDir(elsewhere)
	require.NoError(t, err)
	require.Empty(t, entries)
}
