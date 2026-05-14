//go:build !linux

package vm

func init() {
	newStatsCollector = func(pid int, v *VM) StatsCollector {
		return NoopStatsCollector{ID: v.ID, State: string(v.GetState())}
	}
}
