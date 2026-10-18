package main

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceDateEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "")
	require.Empty(t, sourceDateEpoch())
	t.Setenv("SOURCE_DATE_EPOCH", "1767225600")
	require.Equal(t, "2026-01-01T00:00:00Z", sourceDateEpoch())
	t.Setenv("SOURCE_DATE_EPOCH", "yesterday")
	require.Empty(t, sourceDateEpoch())
	t.Setenv("SOURCE_DATE_EPOCH", "-5")
	require.Empty(t, sourceDateEpoch())
}

func TestResolveLayout(t *testing.T) {
	require.Equal(t, "standard", resolveLayout("standard", "arm64"))
	require.Equal(t, "compact", resolveLayout("compact", "amd64"))
	require.Equal(t, "compact", resolveLayout("", "arm64"))
	if runtime.GOOS == "darwin" {
		require.Equal(t, "standard", resolveLayout("", "amd64"), "x86 images on macOS run under x86 emulation")
	} else {
		require.Equal(t, "compact", resolveLayout("", "amd64"))
	}
}
