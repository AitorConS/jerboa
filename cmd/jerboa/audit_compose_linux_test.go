package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditComposePartialDeploymentRecoverable(t *testing.T) {
	client, endpoint := startComposeDaemon(t)
	store := filepath.Join(t.TempDir(), "images")
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	require.NoError(t, os.WriteFile(disk, []byte("fake"), 0o600))
	file := filepath.Join(dir, "partial.yml")
	content := fmt.Sprintf("version: \"1\"\nservices:\n  a:\n    image: %s\n  b:\n    image: missing:latest\n    depends_on: [a]\n", disk)
	require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
	up := newComposeUpCmd(&endpoint, &store)
	up.SetArgs([]string{file})
	require.Error(t, up.Execute())
	state, err := readState(file)
	require.NoError(t, err)
	require.Len(t, state.Services, 1)
	down := newComposeDownCmd(&endpoint, &store)
	down.SetArgs([]string{file})
	require.NoError(t, down.Execute())
	require.NoError(t, down.Execute())
	all, err := client.List(context.Background())
	require.NoError(t, err)
	require.Empty(t, all)
}
