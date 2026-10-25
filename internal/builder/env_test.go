package builder

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvDropsJerboaCredentials(t *testing.T) {
	t.Setenv("JERBOA_REGISTRY_TOKEN", "secret")
	t.Setenv("JERBOA_AUTH_TOKEN", "secret")
	t.Setenv("JERBOA_BUILD_TEST_VAR", "kept")
	env := Env("EXTRA=1")
	require.NotContains(t, env, "JERBOA_REGISTRY_TOKEN=secret")
	require.NotContains(t, env, "JERBOA_AUTH_TOKEN=secret")
	require.Contains(t, env, "JERBOA_BUILD_TEST_VAR=kept")
	require.Equal(t, "EXTRA=1", env[len(env)-1])
}
