//go:build linux

package vm

func init() {
	newStatsCollector = newProcStatsCollector
}
