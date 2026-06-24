//go:build linux

package main

import (
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/require"
)

func TestNodeListCmd_Disabled(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	_ = execRootExpectError(t, socketPath, storePath, "node", "ls")
	_ = client
}

func TestNodeListResponse_Fields(t *testing.T) {
	resp := api.NodeListResponse{
		Nodes: []api.NodeRow{
			{ID: "node-1", Addr: "10.0.0.1:7946", Status: "alive", VMCount: 3, CPUCap: 8, MemCap: 17179869184, LastSeen: "2026-05-16T10:00:00Z"},
		},
	}
	require.Len(t, resp.Nodes, 1)
	require.Equal(t, "node-1", resp.Nodes[0].ID)
	require.Equal(t, "alive", resp.Nodes[0].Status)
	require.Equal(t, int64(17179869184), resp.Nodes[0].MemCap)
}
