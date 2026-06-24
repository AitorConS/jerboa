package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// setHome points os.UserHomeDir at a temp directory on both Unix and Windows so
// DefaultPath resolves into an isolated location.
func setHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

func TestDefaultEndpoint(t *testing.T) {
	ep := DefaultEndpoint()
	if runtime.GOOS == "windows" {
		require.Equal(t, "tcp://127.0.0.1:7890", ep)
	} else {
		require.Equal(t, "unix:///var/run/jerboad.sock", ep)
	}
}

func TestDefaultPath(t *testing.T) {
	setHome(t)
	p := DefaultPath()
	require.Contains(t, p, ".jerboa")
	require.Contains(t, filepath.Base(p), "config.toml")
}

func TestResolveEndpoint_OverrideWins(t *testing.T) {
	t.Setenv("JERBOA_HOST", "tcp://10.0.0.1:1234")
	require.Equal(t, "tcp://override:1", ResolveEndpoint("tcp://override:1"))
}

func TestResolveEndpoint_EnvVar(t *testing.T) {
	setHome(t)
	t.Setenv("JERBOA_HOST", "tcp://env:7890")
	require.Equal(t, "tcp://env:7890", ResolveEndpoint(""))
}

func TestResolveEndpoint_ConfigFile(t *testing.T) {
	setHome(t)
	t.Setenv("JERBOA_HOST", "")
	require.NoError(t, Save(DefaultPath(), &Config{
		Hypervisor: "qemu",
		Daemon:     DaemonConfig{Endpoint: "tcp://file:7890"},
	}))
	require.Equal(t, "tcp://file:7890", ResolveEndpoint(""))
}

func TestResolveEndpoint_Default(t *testing.T) {
	setHome(t)
	t.Setenv("JERBOA_HOST", "")
	require.Equal(t, DefaultEndpoint(), ResolveEndpoint(""))
}

func TestResolveToken_EnvWins(t *testing.T) {
	setHome(t)
	t.Setenv("JERBOA_AUTH_TOKEN", "env-token")
	require.NoError(t, Save(DefaultPath(), &Config{Daemon: DaemonConfig{Token: "file-token"}}))
	require.Equal(t, "env-token", ResolveToken())
}

func TestResolveToken_ConfigFile(t *testing.T) {
	setHome(t)
	t.Setenv("JERBOA_AUTH_TOKEN", "")
	require.NoError(t, Save(DefaultPath(), &Config{Daemon: DaemonConfig{Token: "file-token"}}))
	require.Equal(t, "file-token", ResolveToken())
}

func TestResolveToken_Empty(t *testing.T) {
	setHome(t)
	t.Setenv("JERBOA_AUTH_TOKEN", "")
	require.Empty(t, ResolveToken())
}

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	require.NoError(t, err)
	require.Equal(t, "qemu", cfg.Hypervisor)
}

func TestLoad_InvalidTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	require.NoError(t, os.WriteFile(path, []byte("this is = = not valid toml ["), 0o600))
	_, err := Load(path)
	require.Error(t, err)
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	want := &Config{
		Hypervisor: "firecracker",
		Daemon: DaemonConfig{
			Endpoint:    "tcp://127.0.0.1:7890",
			Distro:      "jerboa",
			JerboadPath: "/usr/local/bin/jerboad",
			Token:       "secret",
		},
	}
	require.NoError(t, Save(path, want))

	got, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, want.Hypervisor, got.Hypervisor)
	require.Equal(t, want.Daemon, got.Daemon)
}

func TestLoad_EmptyHypervisorDefaultsToQEMU(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("hypervisor = \"\"\n"), 0o600))
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "qemu", cfg.Hypervisor)
}
