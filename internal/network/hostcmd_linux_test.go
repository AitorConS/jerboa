package network

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostCommandHasNoCredentials(t *testing.T) {
	t.Setenv("JERBOA_AUTH_TOKEN", "s3cr3t")
	t.Setenv("JERBOA_CLUSTER_TOKEN", "s3cr3t")
	t.Setenv("JERBOA_OBSERVABILITY_TOKEN", "s3cr3t")
	cmd := hostCommand("ip", "link", "show")
	require.NotNil(t, cmd.Env, "a nil Env would inherit everything")
	for _, kv := range cmd.Env {
		require.NotContains(t, kv, "s3cr3t")
		require.False(t, strings.HasPrefix(kv, "JERBOA_") && strings.Contains(kv, "TOKEN"), kv)
	}
}
