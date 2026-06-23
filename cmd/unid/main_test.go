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
	"github.com/AitorConS/unikernel-engine/internal/config"
	"github.com/stretchr/testify/require"
)

func TestServe_StartsAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "unid-test.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		err := serve(ctx, socketPath, "fake-qemu", t.TempDir(), "file", "", "", "text", "", "", "", "qemu", "", "")
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

func TestDefaultEndpoint(t *testing.T) {
	ep := config.DefaultEndpoint()
	if runtime.GOOS == "windows" {
		require.Equal(t, "tcp://127.0.0.1:7890", ep)
	} else {
		require.Equal(t, "unix:///var/run/unid.sock", ep)
	}
}

func TestDefaultStorePath(t *testing.T) {
	p := defaultStorePath()
	require.Contains(t, p, ".uni")
}

func TestNewRootCmd_Flags(t *testing.T) {
	cmd := newRootCmd()
	require.NotNil(t, cmd.Flag("host"))
	require.NotNil(t, cmd.Flag("socket"))
	require.NotNil(t, cmd.Flag("qemu"))
	require.NotNil(t, cmd.Flag("store"))
	require.NotNil(t, cmd.Flag("vm-store"))
	require.NotNil(t, cmd.Flag("metrics-addr"))
	require.NotNil(t, cmd.Flag("ui-addr"))
	require.NotNil(t, cmd.Flag("log-format"))
	require.NotNil(t, cmd.Flag("trace-addr"))
	require.NotNil(t, cmd.Flag("cluster-addr"))
	require.NotNil(t, cmd.Flag("join"))
	require.NotNil(t, cmd.Flag("hypervisor"))
	require.NotNil(t, cmd.Flag("fc-bin"))
	require.NotNil(t, cmd.Flag("fc-kernel"))
}

func TestServe_VersionQuery(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "unid-ver.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = serve(ctx, socketPath, "fake-qemu", t.TempDir(), "file", "", "", "text", "", "", "", "qemu", "", "")
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
