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

// The minimum length used to apply to the API token only: a short cluster or
// observability token on a network listener could be guessed.
func TestClusterTokenStrength(t *testing.T) {
	require.NoError(t, checkClusterTokenStrength(""), "no token is checkExposure's refusal")
	require.NoError(t, checkClusterTokenStrength("0123456789abcdef0123456789abcdef"))
	err := checkClusterTokenStrength("gossip-token")
	require.ErrorContains(t, err, "cluster token is too short")
	require.ErrorContains(t, err, "openssl rand -hex 32")
	require.ErrorContains(t, err, "every node")
	require.NotContains(t, err.Error(), "gossip-token")
}

func TestObservabilityTokenStrength(t *testing.T) {
	strong := "0123456789abcdef0123456789abcdef"
	// On loopback only local users can try tokens; the check stays out of
	// the way there.
	require.NoError(t, checkObservabilityTokenStrength("127.0.0.1:9090", "localhost:8080", "short", false))
	require.NoError(t, checkObservabilityTokenStrength(":9090", ":8080", strong, false))
	require.NoError(t, checkObservabilityTokenStrength(":9090", "", "short", true), "--insecure allows no token, so a short one too")
	require.NoError(t, checkObservabilityTokenStrength(":9090", "", "", false))

	for _, addrs := range [][2]string{{":9090", ""}, {"", "0.0.0.0:8080"}, {"127.0.0.1:9090", "192.168.1.10:8080"}} {
		err := checkObservabilityTokenStrength(addrs[0], addrs[1], "scrape-token", false)
		require.ErrorContains(t, err, "observability token is too short", addrs)
		require.ErrorContains(t, err, "openssl rand -hex 32")
		require.NotContains(t, err.Error(), "scrape-token")
	}
}
