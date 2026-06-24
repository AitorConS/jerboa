package wslboot

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadOrCreateToken_GeneratesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "daemon.json")

	tok1, err := LoadOrCreateToken(path)
	require.NoError(t, err)
	require.Len(t, tok1, 64) // 32 random bytes, hex-encoded

	require.FileExists(t, path)

	tok2, err := LoadOrCreateToken(path)
	require.NoError(t, err)
	require.Equal(t, tok1, tok2, "token must be stable across calls")
}

func TestBuildLaunchArgs_TokenAndDistro(t *testing.T) {
	args, env := buildLaunchArgs(Config{
		Endpoint:    "tcp://127.0.0.1:7890",
		Distro:      "Ubuntu",
		Token:       "abc123",
		JerboadPath: "jerboad",
	})

	require.Equal(t, []string{"-d", "Ubuntu", "--", "jerboad", "--host", "tcp://127.0.0.1:7890"}, args)
	require.True(t, slices.Contains(env, "JERBOA_AUTH_TOKEN=abc123"))
	require.True(t, slices.Contains(env, "WSLENV=JERBOA_AUTH_TOKEN/u"))
}

func TestBuildLaunchArgs_NoTokenNoDistro(t *testing.T) {
	args, env := buildLaunchArgs(Config{Endpoint: "tcp://127.0.0.1:7890", JerboadPath: "/usr/local/bin/jerboad"})

	require.Equal(t, []string{"--", "/usr/local/bin/jerboad", "--host", "tcp://127.0.0.1:7890"}, args)
	// With no token, buildLaunchArgs must not add anything beyond the inherited
	// process environment (no JERBOA_AUTH_TOKEN / WSLENV injection).
	require.Equal(t, os.Environ(), env)
}

func TestBuildLaunchArgs_HypervisorAndSudo(t *testing.T) {
	args, env := buildLaunchArgs(Config{
		Endpoint:   "tcp://127.0.0.1:7890",
		Token:      "secret",
		Hypervisor: "firecracker",
		Sudo:       true,
	})

	require.Equal(t, []string{
		"--", "sudo", "--preserve-env=JERBOA_AUTH_TOKEN",
		"jerboad", "--host", "tcp://127.0.0.1:7890", "--hypervisor", "firecracker",
	}, args)
	require.True(t, slices.Contains(env, "JERBOA_AUTH_TOKEN=secret"))
}

func TestBuildLaunchArgs_SudoNoToken(t *testing.T) {
	args, _ := buildLaunchArgs(Config{Endpoint: "tcp://127.0.0.1:7890", Sudo: true})
	// Without a token there is nothing to preserve across sudo.
	require.Equal(t, []string{"--", "sudo", "jerboad", "--host", "tcp://127.0.0.1:7890"}, args)
}

func TestBuildLaunchArgs_DedicatedDistro(t *testing.T) {
	args, _ := buildLaunchArgs(Config{
		Endpoint:       "tcp://127.0.0.1:7890",
		ListenEndpoint: "tcp://0.0.0.0:7890",
		Distro:         "jerboa",
		User:           "root",
		Hypervisor:     "firecracker",
	})

	// -u selects the user, and the daemon binds the listen endpoint (0.0.0.0)
	// while the client keeps dialing loopback.
	require.Equal(t, []string{
		"-d", "jerboa", "-u", "root", "--",
		"jerboad", "--host", "tcp://0.0.0.0:7890", "--hypervisor", "firecracker",
	}, args)
}

func TestSaveLoadDaemonFile_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "daemon.json")
	require.NoError(t, SaveDaemonFile(path, "tok", "tcp://127.0.0.1:7890"))

	tok, ep, err := LoadDaemonFile(path)
	require.NoError(t, err)
	require.Equal(t, "tok", tok)
	require.Equal(t, "tcp://127.0.0.1:7890", ep)
}

func TestLoadDaemonFile_Missing(t *testing.T) {
	tok, ep, err := LoadDaemonFile(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, err)
	require.Empty(t, tok)
	require.Empty(t, ep)
}

func TestLoadOrCreateToken_FileModeUnix(t *testing.T) {
	if os.Getenv("SKIP_PERM") != "" {
		t.Skip()
	}
	path := filepath.Join(t.TempDir(), "daemon.json")
	_, err := LoadOrCreateToken(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	// On Windows the Unix permission bits are not enforced; only assert on Unix.
	if os.PathSeparator == '/' {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}
