package vm

import (
	"fmt"
	"math"
	"sync"
	"time"
)

type darwinUsage struct {
	pid, ppid int
	cpuNS     uint64
	start     uint64
	rss, disk int64
}

type darwinProcessIdentity struct {
	pid   int
	start uint64
}

type darwinStats struct {
	mu        sync.Mutex
	lastCPU   map[darwinProcessIdentity]uint64
	lastTime  time.Time
	pid       int
	rootStart uint64
	tree      bool
	vm        *VM
}

var (
	darwinUsageReader    = readDarwinUsage
	darwinChildrenReader = listDarwinChildren
)

func init() {
	newStatsCollector = newDarwinStatsCollector
}

func newDarwinStatsCollector(pid int, v *VM) StatsCollector {
	s := &darwinStats{pid: pid, vm: v, lastCPU: make(map[darwinProcessIdentity]uint64)}
	if usage, err := darwinUsageReader(pid); err == nil {
		s.rootStart = usage.start
	}
	// The macOS Firecracker executable is an API supervisor. Its VMM, network
	// broker and authority are descendants, while native QEMU is accounted as
	// the single process it has always been.
	s.tree = processIsNativeFCSupervisor(pid, v.ID)
	return s
}

func (s *darwinStats) Collect() RuntimeStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := RuntimeStats{ID: s.vm.ID, State: string(s.vm.GetState()), Timestamp: time.Now(), Source: "fallback"}
	if usages, err := s.readUsages(); err == nil {
		current := make(map[darwinProcessIdentity]uint64, len(usages))
		var cpuDelta uint64
		for _, u := range usages {
			identity := darwinProcessIdentity{pid: u.pid, start: u.start}
			current[identity] = u.cpuNS
			if previous, ok := s.lastCPU[identity]; ok {
				if u.cpuNS >= previous {
					cpuDelta += u.cpuNS - previous
				}
			} else if processStartedDuring(u.start, s.lastTime, stats.Timestamp) {
				// Only a descendant proven to have started during this interval may
				// contribute lifetime CPU from zero. An older identity can be absent
				// because of a transient read/enumeration race; its reappearance is a
				// fresh baseline, not a lifetime-sized spike.
				cpuDelta += u.cpuNS
			}
			stats.MemBytes = saturatingAddInt64(stats.MemBytes, u.rss)
			stats.DiskBytes = saturatingAddInt64(stats.DiskBytes, u.disk)
		}
		if elapsed := stats.Timestamp.Sub(s.lastTime); !s.lastTime.IsZero() && elapsed > 0 {
			stats.CPUPct = 100 * float64(cpuDelta) / float64(elapsed)
			stats.CPUPct = math.Min(stats.CPUPct, 100*float64(numCPU()))
		}
		s.lastCPU = current // drop exited identities: bounded by the live tree
		s.lastTime = stats.Timestamp
		stats.Source = darwinStatsSource
		if s.tree {
			stats.Source += "-tree"
		}
		s.vm.mu.RLock()
		networkStats := s.vm.networkStats
		s.vm.mu.RUnlock()
		if networkStats != nil {
			stats.NetRxBytes, stats.NetTxBytes = networkStats()
		}
		return stats
	}
	return stats
}

func processStartedDuring(startMicros uint64, after, before time.Time) bool {
	if after.IsZero() || startMicros > math.MaxInt64 {
		return false
	}
	started := time.UnixMicro(int64(startMicros))
	return !started.Before(after) && !started.After(before)
}

func (s *darwinStats) readUsages() ([]darwinUsage, error) {
	root, err := darwinUsageReader(s.pid)
	if err != nil {
		return nil, err
	}
	if s.rootStart == 0 {
		// A collector whose root identity could not be captured at construction
		// must never attach later to a potentially reused PID.
		return nil, fmt.Errorf("root process identity was unavailable")
	}
	if root.pid != s.pid || root.start != s.rootStart {
		return nil, fmt.Errorf("root process identity changed")
	}
	if !s.tree {
		return []darwinUsage{root}, nil
	}
	result := []darwinUsage{root}
	queue := []darwinUsage{root}
	seen := map[darwinProcessIdentity]bool{{pid: root.pid, start: root.start}: true}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		children, err := darwinChildrenReader(parent.pid)
		if err != nil {
			return nil, err
		}
		for _, pid := range children {
			child, err := darwinUsageReader(pid)
			if err != nil || child.pid != pid || child.ppid != parent.pid || child.start < parent.start {
				// Exits and reparenting are normal while sampling. The parent link
				// and creation time checks prevent a recycled PID from being counted.
				continue
			}
			identity := darwinProcessIdentity{pid: child.pid, start: child.start}
			if seen[identity] {
				continue
			}
			seen[identity] = true
			result = append(result, child)
			queue = append(queue, child)
		}
	}
	return result, nil
}

func saturatingAddInt64(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}
