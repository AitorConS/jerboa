//go:build linux

package main

import (
	"context"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/require"
)

func TestDNSResolveAndList(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	_, err := client.NetworkCreate(context.Background(), "app", "10.100.10.0/24", "bridge")
	require.NoError(t, err)

	_, err = client.Run(context.Background(), api.RunParams{
		ImagePath:   testImage(t),
		Memory:      "256M",
		Name:        "frontend",
		NetworkName: "app",
		IPAddress:   "10.100.10.2",
		GatewayIP:   "10.100.10.1",
		SubnetMask:  "24",
	})
	require.NoError(t, err)

	out := execRoot(t, socketPath, storePath, "dns", "resolve", "frontend", "--network", "app")
	require.Contains(t, out, "10.100.10.2")

	out = execRoot(t, socketPath, storePath, "dns", "list", "--network", "app")
	require.Contains(t, out, "frontend")
}

// A duplicate VM name across two networks is exactly what makes a bare DNS name
// ambiguous. BUG-004 now refuses the second same-name run, so the ambiguous
// condition can no longer be created through the daemon at all. Here we assert
// that guarantee end-to-end; the resolver's own handling of an already-ambiguous
// set of records is unit-tested in internal/scheduler (TestResolverResolveAmbiguous).
func TestDNSResolveDuplicateNameRejected(t *testing.T) {
	client, _ := startDaemon(t)

	_, err := client.NetworkCreate(context.Background(), "app-a", "10.100.11.0/24", "bridge")
	require.NoError(t, err)
	_, err = client.NetworkCreate(context.Background(), "app-b", "10.100.12.0/24", "bridge")
	require.NoError(t, err)

	_, err = client.Run(context.Background(), api.RunParams{
		ImagePath:   testImage(t),
		Memory:      "256M",
		Name:        "api",
		NetworkName: "app-a",
		IPAddress:   "10.100.11.2",
		GatewayIP:   "10.100.11.1",
		SubnetMask:  "24",
	})
	require.NoError(t, err)

	// The second VM reusing the name "api" must be rejected, not booted into an
	// ambiguous DNS state.
	_, err = client.Run(context.Background(), api.RunParams{
		ImagePath:   testImage(t),
		Memory:      "256M",
		Name:        "api",
		NetworkName: "app-b",
		IPAddress:   "10.100.12.2",
		GatewayIP:   "10.100.12.1",
		SubnetMask:  "24",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "already in use")
}
