package childproc

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func setCredentials(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"JERBOA_AUTH_TOKEN", "JERBOA_CLUSTER_TOKEN", "JERBOA_OBSERVABILITY_TOKEN",
		"JERBOA_AGENT_TOKEN", "JERBOA_REGISTRY_TOKEN", "JERBOA_REGISTRY_ACCESS_CLIENT_SECRET",
		"JERBOA_MINISIGN_SEED", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "JERBOA_FUTURE_TOKEN",
	} {
		t.Setenv(name, "s3cr3t")
	}
}

func requireNoCredential(t *testing.T, env []string) {
	t.Helper()
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		require.False(t, IsCredential(name), "child env has credential %s", name)
		require.NotEqual(t, "s3cr3t", value, "child env has a credential in %s", name)
	}
}

func TestIsCredential(t *testing.T) {
	for _, name := range []string{"JERBOA_AUTH_TOKEN", "jerboa_auth_token", "JERBOA_CLUSTER_TOKEN",
		"JERBOA_OBSERVABILITY_TOKEN", "JERBOA_REGISTRY_TOKEN", "JERBOA_MINISIGN_SEED",
		"JERBOA_REGISTRY_ACCESS_CLIENT_SECRET", "ACTIONS_ID_TOKEN_REQUEST_URL"} {
		require.True(t, IsCredential(name), name)
	}
	for _, name := range []string{"PATH", "HOME", "JERBOA_QEMU_SANDBOX", "JERBOA_MKFS_HOST_ROOT", "GITHUB_TOKEN_X"} {
		require.False(t, IsCredential(name), name)
	}
}

func TestInheritedDropsCredentialsOnly(t *testing.T) {
	setCredentials(t)
	t.Setenv("JERBOA_CHILDPROC_TEST", "kept")
	env := Inherited("EXTRA=1")
	requireNoCredential(t, env)
	require.Contains(t, env, "JERBOA_CHILDPROC_TEST=kept")
	require.Equal(t, "EXTRA=1", env[len(env)-1])
}

func TestMinimalKeepsOnlyTheAllowlist(t *testing.T) {
	setCredentials(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/var/lib/jerboa")
	t.Setenv("TMPDIR", "/tmp/x")
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("LC_ALL", "C")
	t.Setenv("JERBOA_CHILDPROC_TEST", "dropped")
	t.Setenv("LD_PRELOAD", "/evil.so")
	env := Minimal("JERBOA_MKFS_HOST_ROOT=/store")
	requireNoCredential(t, env)
	for _, kv := range []string{"PATH=/usr/bin:/bin", "HOME=/var/lib/jerboa", "TMPDIR=/tmp/x", "LANG=C.UTF-8", "LC_ALL=C", "JERBOA_MKFS_HOST_ROOT=/store"} {
		require.Contains(t, env, kv)
	}
	for _, kv := range env {
		require.NotContains(t, kv, "JERBOA_CHILDPROC_TEST")
		require.NotContains(t, kv, "LD_PRELOAD")
	}
}

func TestCommandEnvHasNoCredentials(t *testing.T) {
	setCredentials(t)
	cmd := Command(context.Background(), "mkfs", "out.img")
	require.NotNil(t, cmd.Env, "a nil Env would inherit everything")
	requireNoCredential(t, cmd.Env)
}
