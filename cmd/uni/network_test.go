//go:build linux

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNetworkCreateAndInspect(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	execRoot(t, socketPath, storePath, "network", "create", "mynet")

	net, err := client.NetworkGet(context.Background(), "mynet")
	require.NoError(t, err)
	require.Equal(t, "mynet", net.Name)
	require.Equal(t, "bridge", net.Driver)
	require.NotEmpty(t, net.Subnet)
}

func TestNetworkCreateWithSubnet(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	execRoot(t, socketPath, storePath, "network", "create", "custom", "--subnet", "10.210.0.0/24")

	net, err := client.NetworkGet(context.Background(), "custom")
	require.NoError(t, err)
	require.Equal(t, "10.210.0.0/24", net.Subnet)
}

func TestNetworkList(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	execRoot(t, socketPath, storePath, "network", "create", "net1")
	execRoot(t, socketPath, storePath, "network", "create", "net2")
	execRoot(t, socketPath, storePath, "network", "ls")

	nets, err := client.NetworkList(context.Background())
	require.NoError(t, err)
	require.Len(t, nets, 2)
}

func TestNetworkInspect(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	execRoot(t, socketPath, storePath, "network", "create", "inspect-net")
	execRoot(t, socketPath, storePath, "network", "inspect", "inspect-net")

	net, err := client.NetworkGet(context.Background(), "inspect-net")
	require.NoError(t, err)
	require.Equal(t, "inspect-net", net.Name)
}

func TestNetworkRemove(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	execRoot(t, socketPath, storePath, "network", "create", "gone")
	execRoot(t, socketPath, storePath, "network", "rm", "gone")

	_, err := client.NetworkGet(context.Background(), "gone")
	require.Error(t, err)
}

func TestNetworkRemoveNotFound(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	msg := execRootExpectError(t, socketPath, storePath, "network", "rm", "missing")
	require.Contains(t, msg, "network rm")
}
