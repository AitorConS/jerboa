//go:build linux || (darwin && arm64)

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

	// Read this VM's own tap device, not the hypervisor process's /proc net stats:
	// the hypervisor shares the distro's network namespace, so /proc/<pid>/net/dev
	// reported the distro's eth0 — the same large, meaningless number for every VM
	// (E2E finding F-007). The tap is the per-VM interface, so its counters are the
	// VM's real traffic.
	var rxBytes, txBytes int64
	if tap := c.vm.Cfg.tapDevice(); tap != "" {
		if rx, tx, err := readTapStats(tap); err == nil {
			rxBytes = rx
			txBytes = tx
		}
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

// readTapStats returns a VM's guest-centric byte counters read from its host-side
// tap device statistics. A tap's rx/tx are measured from the host's end of the
// link, so they are flipped to the guest's point of view: what the host received
// off the tap is what the guest SENT (guest TX), and what the host transmitted
// into the tap is what the guest RECEIVED (guest RX).
// sysClassNet is the sysfs network directory; a var so tests can point it at a
// fixture instead of the real /sys.
var sysClassNet = "/sys/class/net"

func readTapStats(tap string) (guestRx, guestTx int64, err error) {
	base := sysClassNet + "/" + tap + "/statistics/"
	hostRx, err := readNetCounter(base + "rx_bytes")
	if err != nil {
		return 0, 0, err
	}
	hostTx, err := readNetCounter(base + "tx_bytes")
	if err != nil {
		return 0, 0, err
	}
	return hostTx, hostRx, nil
}

func readNetCounter(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read net counter %s: %w", path, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse net counter %s: %w", path, err)
	}
	return n, nil
}

func numCPU() int {
	return runtime.NumCPU()
}
