//go:build linux

package vm

func init() {
	newStatsCollector = func(pid int, v *VM) StatsCollector {
		return newProcStatsCollector(pid, v)
	}
}