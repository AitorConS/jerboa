package vm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVM_Stats_Fallback(t *testing.T) {
	v := &VM{
		ID:        "test-vm",
		State:     StateRunning,
		Cfg:       Config{ImagePath: "test.img", Memory: "256M"},
		CreatedAt: time.Now(),
		done:      make(chan struct{}),
	}

	stats := v.Stats()
	require.Equal(t, "test-vm", stats.ID)
	require.Equal(t, "running", stats.State)
	require.Equal(t, "fallback", stats.Source)
	require.WithinDuration(t, time.Now(), stats.Timestamp, 2*time.Second)
}

func TestVM_Stats_WithProvider(t *testing.T) {
	v := &VM{
		ID:        "test-vm",
		State:     StateRunning,
		Cfg:       Config{ImagePath: "test.img", Memory: "256M"},
		CreatedAt: time.Now(),
		done:      make(chan struct{}),
	}

	expected := RuntimeStats{
		ID:         "test-vm",
		State:      "running",
		CPUPct:     42.5,
		MemBytes:   1048576,
		NetRxBytes: 500,
		NetTxBytes: 800,
		Timestamp:  time.Now(),
		Source:     "procfs",
	}

	v.SetStatsProvider(func() RuntimeStats {
		return expected
	})

	stats := v.Stats()
	require.InDelta(t, expected.CPUPct, stats.CPUPct, 1e-9)
	require.Equal(t, expected.MemBytes, stats.MemBytes)
	require.Equal(t, expected.NetRxBytes, stats.NetRxBytes)
	require.Equal(t, expected.NetTxBytes, stats.NetTxBytes)
	require.Equal(t, "procfs", stats.Source)
}

func TestProcStatsCollector_FallbackOnNonLinux(t *testing.T) {
	collector := NoopStatsCollector{ID: "vm1", State: "stopped"}
	stats := collector.Collect()
	require.Equal(t, "vm1", stats.ID)
	require.Equal(t, "stopped", stats.State)
	require.Equal(t, "fallback", stats.Source)
	require.InDelta(t, 0.0, stats.CPUPct, 1e-9)
	require.Equal(t, int64(0), stats.MemBytes)
}
