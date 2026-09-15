package vm

import (
	"fmt"
	"log/slog"
	"math"
	"sync"
	"syscall"
	"time"
)

// Darwin has no cgroup v2. Preserve the resource controls using explicit native
// approximations: scheduler niceness and a sampled resident-memory watchdog.
// Neither is a kernel-enforced cgroup boundary; inspect reports that distinction.
type CgroupManager struct {
	done chan struct{}
	once sync.Once
}

func (c *CgroupManager) Remove() error { c.once.Do(func() { close(c.done) }); return nil }
func IsCgroupV2Available() bool        { return false }
func nativeNice(weight uint64) int {
	if weight >= 10000 {
		return 0
	}
	if weight < 1 {
		return 19
	}
	return int(math.Round(19 * (1 - math.Log10(float64(weight))/4)))
}
func defaultApplyLimits(v *VM, pid int) error {
	if v.Cfg.CPUShares > 10000 {
		return fmt.Errorf("CPU weight must be 1-10000")
	}
	if v.Cfg.CPUShares > 0 {
		nice := nativeNice(v.Cfg.CPUShares)
		if err := syscall.Setpriority(syscall.PRIO_PROCESS, pid, nice); err != nil {
			return fmt.Errorf("native CPU priority: %w", err)
		}
		v.AddWarning(fmt.Sprintf("macOS maps CPU weight %d to nice %d; scheduler priority is not a proportional cgroup guarantee", v.Cfg.CPUShares, nice))
	}
	if v.Cfg.MemoryMax > 0 {
		initial, err := readDarwinUsage(pid)
		if err != nil {
			return fmt.Errorf("native memory watchdog unavailable: %w", err)
		}
		cg := &CgroupManager{done: make(chan struct{})}
		v.mu.Lock()
		v.cgroupMgr = cg
		v.mu.Unlock()
		v.AddWarning("macOS memory-max uses a 100ms resident-memory watchdog; temporary overshoot is possible (not a cgroup hard limit)")
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-cg.done:
					return
				case <-ticker.C:
					u, err := readDarwinUsage(pid)
					if err != nil || u.start != initial.start {
						return
					}
					if u.rss > v.Cfg.MemoryMax {
						v.AddWarning(fmt.Sprintf("memory watchdog stopped VM: resident bytes %d exceeded %d", u.rss, v.Cfg.MemoryMax))
						slog.Warn("native memory limit exceeded", "vm_id", v.ID, "rss", u.rss, "limit", v.Cfg.MemoryMax)
						v.mu.RLock()
						proc := v.proc
						v.mu.RUnlock()
						if proc != nil {
							_ = proc.kill()
						}
						return
					}
				}
			}
		}()
	}
	return nil
}
