package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveSecret(t *testing.T) {
	t.Setenv("JERBOA_TEST_SECRET", "from-env")
	got, err := resolveSecret("", "", "JERBOA_TEST_SECRET")
	require.NoError(t, err)
	require.Equal(t, "from-env", got)

	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte("from-file\n"), 0o600))
	got, err = resolveSecret("", file, "JERBOA_TEST_SECRET")
	require.NoError(t, err)
	require.Equal(t, "from-file", got)

	require.NoError(t, os.Chmod(file, 0o644))
	_, err = resolveSecret("", file, "JERBOA_TEST_SECRET")
	require.ErrorContains(t, err, "readable by other users")
}

func TestCheckTokenStrength(t *testing.T) {
	require.Error(t, checkTokenStrength("tcp://127.0.0.1:7890", "short", false))
	require.NoError(t, checkTokenStrength("tcp://127.0.0.1:7890", "short", true))
	require.NoError(t, checkTokenStrength("unix:///tmp/j.sock", "short", false))
	require.NoError(t, checkTokenStrength("tcp://127.0.0.1:7890", "0123456789abcdef0123456789abcdef", false))
}

func TestCheckExposure(t *testing.T) {
	require.NoError(t, checkExposure("127.0.0.1:9090", "", "", "", "", false, false))
	require.ErrorContains(t, checkExposure("", "localhost:8080", "", "", "", false, false), "--ui-addr")
	require.NoError(t, checkExposure("", "127.0.0.1:8080", "obs-token", "", "", false, false))
	require.NoError(t, checkExposure("", "127.0.0.1:8080", "", "", "", true, false))
	require.ErrorContains(t, checkExposure(":9090", "", "", "", "", false, false), "--metrics-addr")
	require.ErrorContains(t, checkExposure("", "0.0.0.0:8080", "", "", "", false, false), "--ui-addr")
	require.NoError(t, checkExposure(":9090", ":8080", "obs-token", "", "", false, false))
	require.NoError(t, checkExposure(":9090", "", "", "", "", true, false))
	require.ErrorContains(t, checkExposure("", "", "", ":7946", "", false, false), "cluster")
	require.NoError(t, checkExposure("", "", "", ":7946", "cluster-token", false, false))
	require.NoError(t, checkExposure("", "", "", ":7946", "", false, true))
}
