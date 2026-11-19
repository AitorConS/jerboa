//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A VMM runs a guest that may take it over: it gets a minimal environment,
// without the daemon's tokens or anything else the daemon was given.
func TestVMMCommandEnvironmentHasNoDaemonSecrets(t *testing.T) {
	for _, name := range []string{"JERBOA_AUTH_TOKEN", "JERBOA_CLUSTER_TOKEN", "JERBOA_OBSERVABILITY_TOKEN", "JERBOA_REGISTRY_TOKEN"} {
		t.Setenv(name, "s3cr3t")
	}
	t.Setenv("JERBOA_GUEST_HOST_ACCESS", "1")
	cmd := NewQEMUManager("qemu-system-x86_64").mkCmd(context.Background(), "qemu-system-x86_64", "-m", "256")
	require.NotNil(t, cmd.Env, "a nil Env would inherit the daemon's environment")
	for _, kv := range cmd.Env {
		require.NotContains(t, kv, "s3cr3t")
		require.False(t, strings.HasPrefix(kv, "JERBOA_"), "VMM env has %q", kv)
	}
}
