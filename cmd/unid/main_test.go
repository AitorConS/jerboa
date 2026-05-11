package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/api"
	"github.com/stretchr/testify/require"
)

func TestServe_StartsAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "unid-test.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		err := serve(ctx, socketPath, "fake-qemu", "", "", "", t.TempDir())
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Logf("serve returned: %v", err)
		}
	}()

	require.Eventually(t, func() bool {
		client, err := api.Dial(socketPath)
		if err != nil {
			return false
		}
		_ = client.Close()
		return true
	}, 5*time.Second, 50*time.Millisecond, "daemon did not start")

	cancel()
}

func TestServe_WithRegistry(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "unid-reg-test.sock")
	storePath := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = serve(ctx, socketPath, "fake-qemu", "127.0.0.1:0", "", "", storePath)
	}()

	require.Eventually(t, func() bool {
		client, err := api.Dial(socketPath)
		if err != nil {
			return false
		}
		_ = client.Close()
		return true
	}, 5*time.Second, 50*time.Millisecond, "daemon did not start")

	cancel()
}

func TestDefaultSocketPath(t *testing.T) {
	p := defaultSocketPath()
	if runtime.GOOS == "windows" {
		require.Contains(t, p, "unid.sock")
	} else {
		require.Contains(t, p, "/var/run/unid.sock")
	}
}

func TestDefaultStorePath(t *testing.T) {
	p := defaultStorePath()
	require.Contains(t, p, ".uni")
}

func TestNewRootCmd_Flags(t *testing.T) {
	cmd := newRootCmd()
	require.NotNil(t, cmd.Flag("socket"))
	require.NotNil(t, cmd.Flag("qemu"))
	require.NotNil(t, cmd.Flag("registry-addr"))
	require.NotNil(t, cmd.Flag("registry-token"))
	require.NotNil(t, cmd.Flag("registry-jwt-secret"))
	require.NotNil(t, cmd.Flag("store"))
}

func TestServe_VersionQuery(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "unid-ver.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = serve(ctx, socketPath, "fake-qemu", "", "", "", t.TempDir())
	}()

	require.Eventually(t, func() bool {
		client, err := api.Dial(socketPath)
		if err != nil {
			return false
		}
		ver, err := client.DaemonVersion(context.Background())
		_ = client.Close()
		return err == nil && ver != ""
	}, 5*time.Second, 50*time.Millisecond, "daemon did not start")

	cancel()
}

func TestNewRootCmd_Execute_Help(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--help"})
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	err := cmd.Execute()
	require.NoError(t, err)
}
