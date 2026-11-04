package credentials

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// noActions clears the GitHub Actions OIDC variables, which CI may set.
func noActions(t *testing.T) {
	t.Setenv(EnvActionsTokenURL, "")
	t.Setenv(EnvActionsToken, "")
}

func TestSaveLoadAndPrecedence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvToken, "")
	noActions(t)
	_, err := For("registry.jerboa.dev")
	require.ErrorContains(t, err, "jerboa login registry.jerboa.dev")

	f, err := Load()
	require.NoError(t, err)
	f.Registries["registry.jerboa.dev"] = Entry{Token: "jrt_stored", Login: "alice"}
	require.NoError(t, f.Save())
	p, err := Path()
	require.NoError(t, err)
	info, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dir, err := os.Stat(filepath.Dir(p))
	require.NoError(t, err)
	require.True(t, dir.IsDir())

	got, err := For("registry.jerboa.dev")
	require.NoError(t, err)
	require.Equal(t, "jrt_stored", got)
	t.Setenv(EnvToken, "jpat_ci")
	got, err = For("registry.jerboa.dev")
	require.NoError(t, err)
	require.Equal(t, "jpat_ci", got)
}

func TestEnvTokenGoesOnlyToItsRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noActions(t)
	t.Setenv(EnvToken, "jpat_ci")
	t.Setenv(EnvHost, "")

	_, err := For("evil.example")
	require.ErrorContains(t, err, EnvHost+"=evil.example")

	t.Setenv(EnvHost, "staging.registry.jerboa.dev")
	got, err := For("staging.registry.jerboa.dev")
	require.NoError(t, err)
	require.Equal(t, "jpat_ci", got)
	_, err = For("registry.jerboa.dev")
	require.Error(t, err)
}

func TestGitHubActionsOIDC(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvToken, "")
	t.Setenv(EnvActionsTokenURL, "https://actions.example/token?x=1")
	t.Setenv(EnvActionsToken, "request-token")
	// Before any stored login, and for any registry: the OIDC token names it as audience.
	f, err := Load()
	require.NoError(t, err)
	f.Registries["registry.jerboa.dev"] = Entry{Token: "jrt_stored"}
	require.NoError(t, f.Save())
	for _, host := range []string{"registry.jerboa.dev", "staging.registry.jerboa.dev"} {
		got, err := For(host)
		require.NoError(t, err)
		require.Equal(t, GitHubActions, got)
	}
	// JERBOA_REGISTRY_TOKEN still wins, and is still kept to its registry.
	t.Setenv(EnvToken, "jpat_ci")
	got, err := For("registry.jerboa.dev")
	require.NoError(t, err)
	require.Equal(t, "jpat_ci", got)
	// Without id-token: write, GitHub sets no request token.
	t.Setenv(EnvToken, "")
	t.Setenv(EnvActionsToken, "")
	got, err = For("registry.jerboa.dev")
	require.NoError(t, err)
	require.Equal(t, "jrt_stored", got)
}
