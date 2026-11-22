package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	hypervisorpkg "github.com/AitorConS/jerboa/internal/hypervisor"
)

func TestNativeSparkLaunchArgs(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "spark & hvf")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("JERBOA_SPARK_BIN", bin)
	t.Setenv("JERBOA_SPARK_SECURITY", filepath.Join(dir, "policy.json"))
	args, err := nativeHypervisorArgs(hypervisorpkg.Firecracker, dir)
	require.NoError(t, err)
	require.Contains(t, args, "<string>--hypervisor</string><string>spark</string>")
	require.Contains(t, args, "<string>--spark-bin</string><string>"+xmlText(bin))
	require.Contains(t, args, "spark &amp; hvf")
	require.Contains(t, args, "<string>--spark-security</string>")
	require.NotContains(t, args, "firecracker")
	require.NotContains(t, args, "--qemu")
	_, err = nativeHypervisorArgs("invalid", dir)
	require.Error(t, err)
	t.Setenv("JERBOA_SPARK_BIN", filepath.Join(dir, "missing"))
	_, err = nativeHypervisorArgs(hypervisorpkg.Firecracker, dir)
	require.ErrorContains(t, err, "executable missing")
}

// The variables' names before the rename to Spark still work.
func TestNativeSparkLaunchArgsLegacyEnvironment(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "vmm")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("JERBOA_SPARK_BIN", "")
	t.Setenv("JERBOA_SPARK_SECURITY", "")
	t.Setenv("JERBOA_FIRECRACKER_BIN", bin)
	t.Setenv("JERBOA_FIRECRACKER_SECURITY", filepath.Join(dir, "policy.json"))
	args, err := nativeHypervisorArgs(hypervisorpkg.Firecracker, dir)
	require.NoError(t, err)
	require.Contains(t, args, "<string>--spark-bin</string><string>"+xmlText(bin))
	require.Contains(t, args, "<string>--spark-security</string>")
}

func TestNativeLifecycleRefusesRemoteOverride(t *testing.T) {
	for _, action := range []string{"start", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("JERBOA_HOST", "")
			root := newRootCmd()
			root.SetArgs([]string{"--host", "tcp://127.0.0.1:1", "daemon", action})
			require.ErrorContains(t, root.Execute(), "requires the default local socket")
		})
	}
}

func TestNativeLaunchdEscapesXML(t *testing.T) {
	require.Equal(t, "a&lt;&amp;&#34;b", xmlText("a<&\"b"))
}
