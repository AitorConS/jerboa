package builder

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvDropsJerboaCredentials(t *testing.T) {
	t.Setenv("JERBOA_REGISTRY_TOKEN", "secret")
	t.Setenv("JERBOA_AUTH_TOKEN", "secret")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "secret")
	t.Setenv("JERBOA_BUILD_TEST_VAR", "kept")
	env := Env("EXTRA=1")
	require.NotContains(t, env, "ACTIONS_ID_TOKEN_REQUEST_TOKEN=secret")
	require.NotContains(t, env, "JERBOA_REGISTRY_TOKEN=secret")
	require.NotContains(t, env, "JERBOA_AUTH_TOKEN=secret")
	require.Contains(t, env, "JERBOA_BUILD_TEST_VAR=kept")
	require.Equal(t, "EXTRA=1", env[len(env)-1])
}

// Build steps run the project's code with network access: well-known
// tokens and keys in the caller's environment do not reach them, while
// what package managers read to fetch private dependencies does.
func TestEnvDropsWellKnownSecrets(t *testing.T) {
	dropped := []string{
		"GITHUB_TOKEN", "GH_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"GOOGLE_APPLICATION_CREDENTIALS", "AZURE_CLIENT_SECRET", "AZURE_CLIENT_CERTIFICATE_PASSWORD",
		"AZURE_STORAGE_KEY", "VAULT_TOKEN", "CLOUDFLARE_API_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY",
		"JERBOA_SOME_FUTURE_TOKEN", "JERBOA_REGISTRY_ACCESS_CLIENT_SECRET", "jerboa_lower_token",
	}
	kept := []string{"NODE_AUTH_TOKEN", "NPM_TOKEN", "CARGO_REGISTRIES_MY_TOKEN", "GOPRIVATE", "PIP_INDEX_URL", "AZURE_CLIENT_ID", "PATH_TEST_KEPT"}
	for _, k := range append(append([]string{}, dropped...), kept...) {
		t.Setenv(k, "value-of-"+k)
	}
	env := Env()
	for _, k := range dropped {
		require.NotContains(t, env, k+"=value-of-"+k)
	}
	for _, k := range kept {
		require.Contains(t, env, k+"=value-of-"+k)
	}
}
