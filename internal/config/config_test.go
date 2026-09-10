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
	} else if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		require.Equal(t, "unix://"+filepath.Join(home, ".jerboa", "jerboad.sock"), ep)
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

func TestSaveTightensExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not portable on Windows")
	}
	dir := filepath.Join(t.TempDir(), "sub")
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(path, []byte("hypervisor = \"qemu\"\n"), 0o644))

	require.NoError(t, Save(path, &Config{Daemon: DaemonConfig{Token: "secret"}}))

	dirInfo, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
	fileInfo, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())
}

func TestLoad_EmptyHypervisorDefaultsToQEMU(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("hypervisor = \"\"\n"), 0o600))
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "qemu", cfg.Hypervisor)
}

// TestSave_OmitsEmptyDaemonTable is the F-002 regression: setting an unrelated
// key must not materialize a [daemon] table full of blank endpoint/token strings
// (which read as "explicitly set to empty" and could shadow the real rendezvous).
func TestSave_OmitsEmptyDaemonTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, Save(path, &Config{Hypervisor: "qemu"}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(data)
	require.Contains(t, got, "hypervisor")
	require.NotContains(t, got, "[daemon]")
	require.NotContains(t, got, "endpoint")
	require.NotContains(t, got, "token")
}

// TestSave_PreservesNonEmptyDaemonFields confirms omitempty drops only blank
// fields: a real token round-trips, while its empty siblings are not written.
func TestSave_PreservesNonEmptyDaemonFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := &Config{Hypervisor: "firecracker"}
	cfg.Daemon.Token = "s3cret"
	require.NoError(t, Save(path, cfg))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(data)
	require.Contains(t, got, "token = 's3cret'")
	require.NotContains(t, got, "endpoint")

	reloaded, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "s3cret", reloaded.Daemon.Token)
	require.Equal(t, "firecracker", reloaded.Hypervisor)
}
