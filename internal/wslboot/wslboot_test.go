package wslboot

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		Endpoint: "tcp://127.0.0.1:7890",
		Distro:   "Ubuntu",
		Token:    "abc123",
		UnidPath: "jerboad",
	})

	require.Equal(t, []string{"-d", "Ubuntu", "--", "jerboad", "--host", "tcp://127.0.0.1:7890"}, args)
	require.True(t, slices.Contains(env, "UNI_AUTH_TOKEN=abc123"))
	require.True(t, slices.Contains(env, "WSLENV=UNI_AUTH_TOKEN/u"))
}

func TestBuildLaunchArgs_NoTokenNoDistro(t *testing.T) {
	args, env := buildLaunchArgs(Config{Endpoint: "tcp://127.0.0.1:7890", UnidPath: "/usr/local/bin/jerboad"})

	require.Equal(t, []string{"--", "/usr/local/bin/jerboad", "--host", "tcp://127.0.0.1:7890"}, args)
	for _, e := range env {
		require.False(t, strings.HasPrefix(e, "UNI_AUTH_TOKEN="), "token must not be set when empty")
		require.False(t, strings.HasPrefix(e, "WSLENV="))
	}
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
