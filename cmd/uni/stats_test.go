package main

import (
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/stretchr/testify/require"
)

func TestStatsCmd_Draft(t *testing.T) {
	cmd := newStatsCmd(ptrStr("/tmp/test.sock"), ptrStr("table"))
	require.Equal(t, "stats <id>", cmd.Use)
	require.True(t, cmd.HasFlags())
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1572864, "1.5 MiB"},
		{1073741824, "1.0 GiB"},
	}
	for _, tc := range tests {
		result := formatBytes(tc.input)
		require.Equal(t, tc.expected, result)
	}
}

func ptrStr(s string) *string {
	return &s
}

func TestStatsCmd_WatchFlag(t *testing.T) {
	cmd := newStatsCmd(ptrStr("/tmp/test.sock"), ptrStr("json"))
	watch := cmd.Flags().Lookup("watch")
	require.NotNil(t, watch)
	interval := cmd.Flags().Lookup("interval")
	require.NotNil(t, interval)
	require.Equal(t, "2s", interval.DefValue)
}

func TestVMStatsResponse_Fields(t *testing.T) {
	s := api.VMStatsResponse{
		ID:         "abc123",
		State:      "running",
		CPUPct:     12.5,
		MemBytes:   268435456,
		NetRxBytes: 1024,
		NetTxBytes: 2048,
		Timestamp:  "2026-05-14T12:00:00Z",
		Source:     "procfs",
	}
	require.Equal(t, "abc123", s.ID)
	require.Equal(t, "running", s.State)
	require.InDelta(t, 12.5, s.CPUPct, 1e-9)
	require.Equal(t, int64(268435456), s.MemBytes)
}
