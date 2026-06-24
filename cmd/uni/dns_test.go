//go:build linux

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/stretchr/testify/require"
)

func TestDNSResolveAndList(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	_, err := client.NetworkCreate(context.Background(), "app", "10.100.10.0/24", "bridge")
	require.NoError(t, err)

	_, err = client.Run(context.Background(), api.RunParams{
		ImagePath:   "test.img",
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

func TestDNSResolveAmbiguous(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	_, err := client.NetworkCreate(context.Background(), "app-a", "10.100.11.0/24", "bridge")
	require.NoError(t, err)
	_, err = client.NetworkCreate(context.Background(), "app-b", "10.100.12.0/24", "bridge")
	require.NoError(t, err)

	_, err = client.Run(context.Background(), api.RunParams{
		ImagePath:   "test.img",
		Memory:      "256M",
		Name:        "api",
		NetworkName: "app-a",
		IPAddress:   "10.100.11.2",
		GatewayIP:   "10.100.11.1",
		SubnetMask:  "24",
	})
	require.NoError(t, err)
	_, err = client.Run(context.Background(), api.RunParams{
		ImagePath:   "test.img",
		Memory:      "256M",
		Name:        "api",
		NetworkName: "app-b",
		IPAddress:   "10.100.12.2",
		GatewayIP:   "10.100.12.1",
		SubnetMask:  "24",
	})
	require.NoError(t, err)

	msg := execRootExpectError(t, socketPath, storePath, "dns", "resolve", "api")
	require.True(t, strings.Contains(msg, "ambiguous") || strings.Contains(msg, "rpc error"))
}
