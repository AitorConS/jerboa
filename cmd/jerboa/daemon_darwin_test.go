package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
