package vm

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var newStatsCollector func(pid int, v *VM) StatsCollector

// StatsCollector provides runtime resource usage for a VM process.
type StatsCollector interface {
	Collect() RuntimeStats
}

// NoopStatsCollector returns fallback stats when no collector is available.
type NoopStatsCollector struct {
	ID    string
	State string
}

func (n NoopStatsCollector) Collect() RuntimeStats {
	return RuntimeStats{
		ID:        n.ID,
		State:     n.State,
		Timestamp: time.Now(),
		Source:    "fallback",
	}
}

// ProcStatsCollector reads /proc/[pid]/ io files for CPU, memory, and I/O stats.
type ProcStatsCollector struct {
	mu       sync.Mutex
	pid      int
	vm       *VM
	lastCPU  uint64
	lastTime time.Time
}

//nolint:unused // used by stats_linux.go on Linux builds
func newProcStatsCollector(pid int, v *VM) StatsCollector {
	return &ProcStatsCollector{
		pid:      pid,
		vm:       v,
		lastTime: time.Now(),
	}
}

func (c *ProcStatsCollector) Collect() RuntimeStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(c.lastTime).Seconds()
	if elapsed < 0.01 {
		elapsed = 0.01
	}

	cpuPct := 0.0
	if totalJiffies, err := readProcStat(c.pid); err == nil {
		if c.lastCPU > 0 {
			delta := float64(totalJiffies-c.lastCPU) / elapsed
			cpuPct = delta * 100.0
			if cpuPct > 100.0*float64(numCPU()) {
				cpuPct = 100.0 * float64(numCPU())
			}
		}
		c.lastCPU = totalJiffies
	}
	c.lastTime = now

	var memBytes int64
	if memKB, err := readProcStatm(c.pid); err == nil {
		memBytes = memKB * 1024
	}

	var rxBytes, txBytes int64
	if rx, tx, err := readProcNetDev(c.pid); err == nil {
		rxBytes = rx
		txBytes = tx
	}

	return RuntimeStats{
		ID:         c.vm.ID,
		State:      string(c.vm.GetState()),
		CPUPct:     cpuPct,
		MemBytes:   memBytes,
		NetRxBytes: rxBytes,
		NetTxBytes: txBytes,
		Timestamp:  now,
		Source:     "procfs",
	}
}

func readProcStat(pid int) (totalJiffies uint64, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("read proc stat: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 17 {
		return 0, fmt.Errorf("unexpected proc stat format")
	}
	utime, _ := strconv.ParseUint(fields[13], 10, 64)
	stime, _ := strconv.ParseUint(fields[14], 10, 64)
	return utime + stime, nil
}

func readProcStatm(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, fmt.Errorf("read proc statm: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected proc statm format")
	}
	rssPages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse rss: %w", err)
	}
	return rssPages * 4, nil
}

func readProcNetDev(pid int) (rxBytes, txBytes int64, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/dev", pid))
	if err != nil {
		return 0, 0, fmt.Errorf("read proc net dev: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "eth0:") || strings.HasPrefix(line, "en") {
			fields := strings.Fields(line)
			if len(fields) < 11 {
				continue
			}
			rx, _ := strconv.ParseInt(fields[1], 10, 64)
			tx, _ := strconv.ParseInt(fields[9], 10, 64)
			return rx, tx, nil
		}
	}
	return 0, 0, nil
}

func numCPU() int {
	return runtime.NumCPU()
}
