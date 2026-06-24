//go:build linux

package main

import (
	"context"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/require"
)

func TestStatusEmpty(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "status")
	require.Contains(t, out, "Total:")
	require.Contains(t, out, "Running:")
	require.Contains(t, out, "Healthy:")
}

func TestStatusWithVM(t *testing.T) {
	client, socketPath := startDaemon(t)
	storePath := t.TempDir()

	info, err := client.Run(context.Background(), api.RunParams{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	out := execRoot(t, socketPath, storePath, "status")
	require.Contains(t, out, "Total:")
	require.Contains(t, out, "Running:")
	require.Contains(t, out, info.ID)
}

func TestStatusJSON(t *testing.T) {
	_, socketPath := startDaemon(t)
	storePath := t.TempDir()

	out := execRoot(t, socketPath, storePath, "--output", "json", "status")
	require.Contains(t, out, "\"total\"")
	require.Contains(t, out, "\"vms\"")
}
