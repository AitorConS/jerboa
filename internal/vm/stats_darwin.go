package vm

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type darwinUsage struct {
	cpuNS     uint64
	start     uint64
	rss, disk int64
}

type darwinStats struct {
	mu       sync.Mutex
	lastCPU  uint64
	lastTime time.Time
	pid      int
	vm       *VM
}

func init() {
	newStatsCollector = func(pid int, v *VM) StatsCollector { return &darwinStats{pid: pid, vm: v} }
}

func (s *darwinStats) Collect() RuntimeStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := RuntimeStats{ID: s.vm.ID, State: string(s.vm.GetState()), Timestamp: time.Now(), Source: "fallback"}
	if u, err := readDarwinUsage(s.pid); err == nil {
		if !s.lastTime.IsZero() && u.cpuNS >= s.lastCPU {
			stats.CPUPct = 100 * float64(u.cpuNS-s.lastCPU) / float64(stats.Timestamp.Sub(s.lastTime))
		}
		s.lastCPU = u.cpuNS
		s.lastTime = stats.Timestamp
		stats.MemBytes, stats.DiskBytes, stats.Source = u.rss, u.disk, darwinStatsSource
		s.vm.mu.RLock()
		networkStats := s.vm.networkStats
		s.vm.mu.RUnlock()
		if networkStats != nil {
			stats.NetRxBytes, stats.NetTxBytes = networkStats()
		}
		return stats
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(s.pid), "-o", "%cpu=,rss=").Output()
	fields := strings.Fields(string(out))
	if err != nil || len(fields) != 2 {
		return stats
	}
	cpu, cpuErr := strconv.ParseFloat(fields[0], 64)
	rss, rssErr := strconv.ParseInt(fields[1], 10, 64)
	if cpuErr != nil || rssErr != nil {
		return stats
	}
	s.vm.mu.RLock()
	networkStats := s.vm.networkStats
	s.vm.mu.RUnlock()
	if networkStats != nil {
		stats.NetRxBytes, stats.NetTxBytes = networkStats()
	}
	stats.CPUPct, stats.MemBytes, stats.Source = cpu, rss*1024, "darwin-ps"
	return stats
}
