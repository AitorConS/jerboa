package main

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestNeedsDaemon(t *testing.T) {
	// Build a small command tree rooted at "jerboa" to mirror the real CLI: the
	// root and offline groups (config, kernel, …) need no daemon, while remote
	// verbs like "run" — and sign/verify, which resolve an image digest through
	// the daemon (F-019) — do.
	root := &cobra.Command{Use: "jerboa"}

	config := &cobra.Command{Use: "config"}
	configSet := &cobra.Command{Use: "set"}
	config.AddCommand(configSet)

	sign := &cobra.Command{Use: "sign"}
	verify := &cobra.Command{Use: "verify"}
	run := &cobra.Command{Use: "run"}

	// The pkg group is otherwise local-only, but `pkg load` builds and runs an
	// image through the daemon, so it must need the daemon while its siblings
	// (e.g. pkg list) do not.
	pkg := &cobra.Command{Use: "pkg"}
	pkgLoad := &cobra.Command{Use: "load"}
	pkgList := &cobra.Command{Use: "list"}
	pkg.AddCommand(pkgLoad, pkgList)
	root.AddCommand(config, sign, verify, run, pkg)

	require.False(t, needsDaemon(root), "root itself needs no daemon")
	require.False(t, needsDaemon(configSet), "offline group descendants need no daemon")
	require.False(t, needsDaemon(config), "offline group needs no daemon")
	require.True(t, needsDaemon(run), "remote verbs need the daemon")
	require.True(t, needsDaemon(sign), "sign resolves the image digest via the daemon (F-019)")
	require.True(t, needsDaemon(verify), "verify resolves the image digest via the daemon (F-019)")
	require.False(t, needsDaemon(pkgList), "pkg list is local-only")
	require.True(t, needsDaemon(pkgLoad), "pkg load builds and runs through the daemon")
}

func TestVolumeCommandsNeedDaemon(t *testing.T) {
	root := newRootCmd()
	// Exercise the actual command tree: even an unseeded create must resolve
	// the WSL endpoint and token before calling the daemon-owned volume API.
	for _, name := range []string{"create", "ls", "rm", "inspect", "seed", "migrate"} {
		t.Run(name, func(t *testing.T) {
			cmd, _, err := root.Find([]string{"volume", name})
			require.NoError(t, err)
			require.Equal(t, name, cmd.Name())
			require.True(t, needsDaemon(cmd))
		})
	}
}

func TestSigningStorePath(t *testing.T) {
	// Fake the home directory so we can assert the full resolved path rather than
	// a loose suffix (the old check passed for any path ending in ".jerboa",
	// including a bare "/.jerboa"). os.UserHomeDir reads USERPROFILE on Windows
	// and HOME elsewhere, so set both for portability.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	require.Equal(t, home+"/.jerboa", signingStorePath(),
		"signing store must resolve to <home>/.jerboa")
}
