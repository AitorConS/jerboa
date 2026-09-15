//go:build darwin && arm64

package vm

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDarwinStatsTreeTracksIdentitiesWithoutDoubleCounting(t *testing.T) {
	originalUsage, originalChildren := darwinUsageReader, darwinChildrenReader
	t.Cleanup(func() { darwinUsageReader, darwinChildrenReader = originalUsage, originalChildren })

	baseStart := uint64(time.Now().Add(-time.Hour).UnixMicro())
	replacementStart := uint64(time.Now().Add(-500 * time.Millisecond).UnixMicro())
	sample := 0
	darwinUsageReader = func(pid int) (darwinUsage, error) {
		values := []map[int]darwinUsage{
			{
				10: {pid: 10, ppid: 1, start: baseStart, cpuNS: 100_000_000, rss: 10, disk: 1},
				20: {pid: 20, ppid: 10, start: baseStart + 1, cpuNS: 50_000_000, rss: 20, disk: 2},
				30: {pid: 30, ppid: 20, start: baseStart + 2, cpuNS: 25_000_000, rss: 30, disk: 3},
			},
			{
				10: {pid: 10, ppid: 1, start: baseStart, cpuNS: 110_000_000, rss: 11, disk: 2},
				20: {pid: 20, ppid: 10, start: baseStart + 1, cpuNS: 70_000_000, rss: 22, disk: 4},
				30: {pid: 30, ppid: 20, start: baseStart + 2, cpuNS: 55_000_000, rss: 33, disk: 6},
			},
			{
				10: {pid: 10, ppid: 1, start: baseStart, cpuNS: 120_000_000, rss: 12, disk: 3},
				20: {pid: 20, ppid: 10, start: replacementStart, cpuNS: 7_000_000, rss: 44, disk: 8},
			},
		}
		u, ok := values[sample][pid]
		if !ok {
			return darwinUsage{}, errors.New("exited")
		}
		return u, nil
	}
	darwinChildrenReader = func(pid int) ([]int, error) {
		switch pid {
		case 10:
			return []int{20, 20}, nil // a duplicate must not double count
		case 20:
			if sample < 2 {
				return []int{30}, nil
			}
		}
		return nil, nil
	}

	s := &darwinStats{
		pid: 10, rootStart: baseStart, tree: true, vm: &VM{ID: "tree", State: StateRunning},
		lastCPU: make(map[darwinProcessIdentity]uint64),
	}
	first := s.Collect()
	require.Equal(t, int64(60), first.MemBytes)
	require.Equal(t, int64(6), first.DiskBytes)
	require.Zero(t, first.CPUPct)
	require.Len(t, s.lastCPU, 3)

	sample = 1
	s.lastTime = time.Now().Add(-time.Second)
	second := s.Collect()
	require.Equal(t, int64(66), second.MemBytes)
	require.InDelta(t, 6.0, second.CPUPct, 0.5)

	// PID 20 exited and was reused. Its new start identity is counted from
	// zero, while the old child/grandchild entries are discarded.
	sample = 2
	s.lastTime = time.Now().Add(-time.Second)
	third := s.Collect()
	require.Equal(t, int64(56), third.MemBytes)
	require.InDelta(t, 1.7, third.CPUPct, 0.5)
	require.Len(t, s.lastCPU, 2)
	require.Contains(t, s.lastCPU, darwinProcessIdentity{pid: 20, start: replacementStart})
	require.NotContains(t, s.lastCPU, darwinProcessIdentity{pid: 20, start: baseStart + 1})
}

func TestDarwinStatsRejectsReusedRootPID(t *testing.T) {
	originalUsage := darwinUsageReader
	t.Cleanup(func() { darwinUsageReader = originalUsage })
	darwinUsageReader = func(pid int) (darwinUsage, error) {
		return darwinUsage{pid: pid, start: 999, cpuNS: 1_000, rss: 1_000}, nil
	}
	s := &darwinStats{
		pid: 42, rootStart: 100, vm: &VM{ID: "reused", State: StateRunning},
		lastCPU: make(map[darwinProcessIdentity]uint64),
	}
	stats := s.Collect()
	require.Equal(t, "fallback", stats.Source)
	require.Zero(t, stats.CPUPct)
	require.Zero(t, stats.MemBytes)
	require.Empty(t, s.lastCPU)
}

func TestDarwinStatsAdoptionStartsWithBaseline(t *testing.T) {
	originalUsage := darwinUsageReader
	t.Cleanup(func() { darwinUsageReader = originalUsage })
	cpu := uint64(500)
	darwinUsageReader = func(pid int) (darwinUsage, error) {
		return darwinUsage{pid: pid, ppid: 1, start: 123, cpuNS: cpu, rss: 4096}, nil
	}
	s := &darwinStats{
		pid: 77, rootStart: 123, vm: &VM{ID: "adopted", State: StateRunning},
		lastCPU: make(map[darwinProcessIdentity]uint64),
	}
	require.Zero(t, s.Collect().CPUPct)
	cpu += 25_000_000
	s.lastTime = time.Now().Add(-100 * time.Millisecond)
	require.InDelta(t, 25.0, s.Collect().CPUPct, 5.0)
}

func TestDarwinStatsReappearingOldChildGetsSafeBaseline(t *testing.T) {
	originalUsage, originalChildren := darwinUsageReader, darwinChildrenReader
	t.Cleanup(func() { darwinUsageReader, darwinChildrenReader = originalUsage, originalChildren })

	now := time.Now()
	oldStart := uint64(now.Add(-time.Hour).UnixMicro())
	sample := 0
	darwinUsageReader = func(pid int) (darwinUsage, error) {
		if pid == 10 {
			return darwinUsage{pid: 10, ppid: 1, start: oldStart, cpuNS: uint64(100+sample*10) * 1_000_000}, nil
		}
		if pid == 20 && sample == 1 {
			return darwinUsage{}, errors.New("transient proc_pid_rusage failure")
		}
		return darwinUsage{pid: 20, ppid: 10, start: oldStart + 1, cpuNS: uint64(500+sample*100) * 1_000_000}, nil
	}
	darwinChildrenReader = func(pid int) ([]int, error) {
		if pid == 10 {
			return []int{20}, nil
		}
		return nil, nil
	}
	s := &darwinStats{
		pid: 10, rootStart: oldStart, tree: true, vm: &VM{ID: "transient", State: StateRunning},
		lastCPU: make(map[darwinProcessIdentity]uint64),
	}
	require.Zero(t, s.Collect().CPUPct)

	sample = 1
	s.lastTime = time.Now().Add(-time.Second)
	second := s.Collect()
	require.InDelta(t, 1.0, second.CPUPct, 0.5)
	require.Len(t, s.lastCPU, 1)

	sample = 2
	s.lastTime = time.Now().Add(-time.Second)
	third := s.Collect()
	require.InDelta(t, 1.0, third.CPUPct, 0.5)
	require.Less(t, third.CPUPct, 10.0, "old child's lifetime CPU must not spike on reappearance")
	require.Len(t, s.lastCPU, 2)
}

func TestDarwinStatsNewReusedPIDCountsOnlyNewLifetime(t *testing.T) {
	intervalStart := time.Now().Add(-time.Second)
	s := &darwinStats{
		lastTime: intervalStart,
		lastCPU: map[darwinProcessIdentity]uint64{
			{pid: 20, start: uint64(intervalStart.Add(-time.Hour).UnixMicro())}: 900_000_000,
		},
	}
	newStart := uint64(intervalStart.Add(500 * time.Millisecond).UnixMicro())
	require.True(t, processStartedDuring(newStart, s.lastTime, time.Now()))
	require.False(t, processStartedDuring(uint64(intervalStart.Add(-time.Hour).UnixMicro()), s.lastTime, time.Now()))
}
