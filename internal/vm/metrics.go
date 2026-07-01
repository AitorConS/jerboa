//go:build linux

package vm

// MetricsSink receives VM lifecycle events for Prometheus counters. Defined
// here rather than accepting *metrics.Collectors directly because
// internal/metrics already imports internal/vm (for state polling), so the
// reverse import would cycle. *metrics.Collectors satisfies this interface
// structurally.
type MetricsSink interface {
	RecordRestart()
	RecordError()
}
