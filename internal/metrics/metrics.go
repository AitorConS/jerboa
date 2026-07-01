//go:build linux

// Package metrics provides Prometheus metrics collection for the unikernel engine daemon.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// VMStateCounts tracks the number of VMs in each state.
type VMStateCounts struct {
	Created  atomic.Int64
	Starting atomic.Int64
	Running  atomic.Int64
	Stopping atomic.Int64
	Stopped  atomic.Int64
}

// Collectors holds all Prometheus metric collectors for the daemon.
type Collectors struct {
	// VM states gauge
	VMCreated  prometheus.Gauge
	VMStarting prometheus.Gauge
	VMRunning  prometheus.Gauge
	VMStopping prometheus.Gauge
	VMStopped  prometheus.Gauge

	// VM lifecycle counters
	VMStartsTotal   prometheus.Counter
	VMStopsTotal    prometheus.Counter
	VMRestartsTotal prometheus.Counter
	VMErrorsTotal   prometheus.Counter

	// Build info
	BuildInfo *prometheus.GaugeVec

	// Registry counts
	ImagesTotal     prometheus.Gauge
	PushTotal       prometheus.Counter
	PullTotal       prometheus.Counter
	PushErrorsTotal prometheus.Counter
	PullErrorsTotal prometheus.Counter

	// Network
	PortForwardsActive prometheus.Gauge
	BridgeCount        prometheus.Gauge

	// Up
	StartTime prometheus.Gauge

	registry *prometheus.Registry
}

// NewCollectors creates and registers all Prometheus metric collectors.
func NewCollectors(version string) *Collectors {
	reg := prometheus.NewRegistry()

	c := &Collectors{
		registry: reg,
		VMCreated: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jerboa_vms_created_total",
			Help: "Number of VMs in created state",
		}),
		VMStarting: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jerboa_vms_starting_total",
			Help: "Number of VMs in starting state",
		}),
		VMRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jerboa_vms_running_total",
			Help: "Number of VMs in running state",
		}),
		VMStopping: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jerboa_vms_stopping_total",
			Help: "Number of VMs in stopping state",
		}),
		VMStopped: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jerboa_vms_stopped_total",
			Help: "Number of VMs in stopped state",
		}),
		VMStartsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jerboa_vm_starts_total",
			Help: "Total number of VM start operations",
		}),
		VMStopsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jerboa_vm_stops_total",
			Help: "Total number of VM stop operations",
		}),
		VMRestartsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jerboa_vm_restarts_total",
			Help: "Total number of VM restart operations",
		}),
		VMErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jerboa_vm_errors_total",
			Help: "Total number of VM errors",
		}),
		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jerboa_build_info",
			Help: "Build information for the unikernel engine daemon",
		}, []string{"version"}),
		ImagesTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jerboa_images_total",
			Help: "Number of locally stored images",
		}),
		PushTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jerboa_push_total",
			Help: "Total number of image push operations",
		}),
		PullTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jerboa_pull_total",
			Help: "Total number of image pull operations",
		}),
		PushErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jerboa_push_errors_total",
			Help: "Total number of image push errors",
		}),
		PullErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jerboa_pull_errors_total",
			Help: "Total number of image pull errors",
		}),
		PortForwardsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jerboa_port_forwards_active",
			Help: "Number of active port forwarding rules",
		}),
		BridgeCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jerboa_bridge_count",
			Help: "Number of active network bridges",
		}),
		StartTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jerboa_start_time_seconds",
			Help: "Unix timestamp of daemon start time",
		}),
	}

	reg.MustRegister(
		c.VMCreated, c.VMStarting, c.VMRunning, c.VMStopping, c.VMStopped,
		c.VMStartsTotal, c.VMStopsTotal, c.VMRestartsTotal, c.VMErrorsTotal,
		c.BuildInfo, c.ImagesTotal,
		c.PushTotal, c.PullTotal, c.PushErrorsTotal, c.PullErrorsTotal,
		c.PortForwardsActive, c.BridgeCount,
		c.StartTime,
	)

	c.BuildInfo.WithLabelValues(version).Set(1)
	c.StartTime.Set(float64(time.Now().Unix()))

	return c
}

// RecordRestart increments the VM restart counter. Satisfies vm.MetricsSink.
func (c *Collectors) RecordRestart() { c.VMRestartsTotal.Inc() }

// RecordError increments the VM error counter. Satisfies vm.MetricsSink.
func (c *Collectors) RecordError() { c.VMErrorsTotal.Inc() }

// Handler returns an http.Handler that serves Prometheus metrics.
func (c *Collectors) Handler() http.Handler {
	return promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// Serve starts an HTTP server serving Prometheus metrics on the given address.
// This is a blocking call; use with a goroutine or context cancellation.
func Serve(ctx context.Context, addr string, c *Collectors) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", c.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	slog.Info("metrics server listening", "addr", addr)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("metrics server shutdown", "err", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}
