package main

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func daemonSubcommand(t *testing.T, name string) *cobra.Command {
	t.Helper()
	for _, c := range newDaemonCmd().Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("daemon subcommand %q not found", name)
	return nil
}

func TestDaemonCmd_Structure(t *testing.T) {
	names := map[string]bool{}
	for _, c := range newDaemonCmd().Commands() {
		names[c.Name()] = true
	}
	for _, want := range []string{"install", "uninstall", "start", "stop", "restart", "status", "logs"} {
		require.True(t, names[want], "missing subcommand %q", want)
	}
}

func TestDaemonWindowsOnly_ErrorsOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows these run the real WSL path")
	}
	for _, name := range []string{"install", "uninstall", "start", "stop", "restart"} {
		cmd := daemonSubcommand(t, name)
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		err := cmd.RunE(cmd, nil)
		require.Error(t, err, name)
		require.Contains(t, err.Error(), "only runs on Windows", name)
	}
}

func TestDaemonStatus_NotRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows status probes the dedicated distro via WSL")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("JERBOA_HOST", "tcp://127.0.0.1:0") // nothing is listening
	t.Setenv("JERBOA_AUTH_TOKEN", "")

	cmd := daemonSubcommand(t, "status")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	require.NoError(t, cmd.RunE(cmd, nil))
	require.Contains(t, buf.String(), "not running")
}

func TestDaemonLogs_Missing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	cmd := daemonSubcommand(t, "logs")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.RunE(cmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no log yet")
}

func TestDaemonPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	t.Setenv("JERBOA_HOST", "tcp://127.0.0.1:9001")
	require.Equal(t, "9001", daemonPort())

	// A non-TCP (or unset) endpoint falls back to the default port.
	t.Setenv("JERBOA_HOST", "unix:///var/run/jerboad.sock")
	require.Equal(t, "7890", daemonPort())
}

func TestDistroListenEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("JERBOA_HOST", "tcp://127.0.0.1:9001")
	require.Equal(t, "tcp://0.0.0.0:9001", distroListenEndpoint())
}

func TestResolveDaemonConfig_NoWSL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows wsl is present and this resolves the distro IP")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("JERBOA_AUTH_TOKEN", "")
	// Without wsl available, IP discovery fails and the config cannot resolve.
	_, _, err := resolveDaemonConfig(daemonOpts{})
	require.Error(t, err)
}

func TestRequireDistro_NoWSL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows wsl reports the real distro list")
	}
	require.Error(t, requireDistro())
}

func TestWaitPortReleased_ReturnsWhenUnreachable(t *testing.T) {
	// Nothing is listening, so the port is already "released" and the helper
	// returns without consuming its full budget.
	start := time.Now()
	waitPortReleased(context.Background(), "tcp://127.0.0.1:1", "", 2*time.Second)
	require.Less(t, time.Since(start), time.Second)
}

func TestErrNotWindows(t *testing.T) {
	require.Contains(t, errNotWindows("start").Error(), "only runs on Windows")
}

func TestDaemonLogPath(t *testing.T) {
	require.Contains(t, daemonLogPath(), filepath.Join(".jerboa", "jerboad-wsl.log"))
}
