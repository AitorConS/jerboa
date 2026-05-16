package vm

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHealthChecker_ProbeLoop_TCP_BecomesHealthy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port

	srv := &http.Server{}
	go srv.Serve(ln)
	defer srv.Close()

	hc := NewHealthChecker()
	v := &VM{
		ID:    "probe-healthy",
		Cfg:   Config{PortMaps: []PortMap{{HostPort: uint16(port), GuestPort: 80, Protocol: ProtocolTCP}}},
		State: StateRunning,
		done:  make(chan struct{}),
	}
	v.Cfg.HealthCheck = &HealthCheckConfig{
		Type:     "tcp",
		Port:     port,
		Interval: 50 * time.Millisecond,
		Timeout:  100 * time.Millisecond,
		Retries:  2,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hc.Start(ctx, v)
	require.Equal(t, HealthStarting, v.GetHealthStatus())

	require.Eventually(t, func() bool {
		return v.GetHealthStatus() == HealthHealthy
	}, 3*time.Second, 50*time.Millisecond)

	hc.Stop(v.ID)
}

func TestHealthChecker_ProbeLoop_TCP_Unhealthy(t *testing.T) {
	hc := NewHealthChecker()
	v := &VM{
		ID:    "probe-unhealthy",
		Cfg:   Config{},
		State: StateRunning,
		done:  make(chan struct{}),
	}
	v.Cfg.HealthCheck = &HealthCheckConfig{
		Type:     "tcp",
		Port:     1,
		Interval: 50 * time.Millisecond,
		Timeout:  50 * time.Millisecond,
		Retries:  2,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hc.Start(ctx, v)

	require.Eventually(t, func() bool {
		return v.GetHealthStatus() == HealthUnhealthy
	}, 5*time.Second, 100*time.Millisecond)

	hc.Stop(v.ID)
}

func TestHealthChecker_ProbeLoop_HTTP_BecomesHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hc := NewHealthChecker()
	v := &VM{
		ID:    "probe-http-healthy",
		Cfg:   Config{},
		State: StateRunning,
		done:  make(chan struct{}),
	}
	v.Cfg.HealthCheck = &HealthCheckConfig{
		Type:     "http",
		Port:     1,
		Path:     "/",
		Interval: 50 * time.Millisecond,
		Timeout:  100 * time.Millisecond,
		Retries:  2,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	target := probeTarget(v, v.Cfg.HealthCheck)
	_ = target

	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	v.Cfg.HealthCheck.Port = port

	hc.Start(ctx, v)

	require.Eventually(t, func() bool {
		return v.GetHealthStatus() == HealthHealthy
	}, 3*time.Second, 50*time.Millisecond)

	hc.Stop(v.ID)
}

func TestHealthChecker_Probe_UnknownType(t *testing.T) {
	hc := NewHealthChecker()
	p := &healthProbe{
		vm: &VM{
			ID:   "unknown-probe",
			done: make(chan struct{}),
		},
		cfg:  HealthCheckConfig{Type: "grpc", Port: 8080, Retries: 1},
		done: make(chan struct{}),
	}
	hc.probe(p)
	require.Equal(t, HealthUnknown, p.vm.GetHealthStatus())
}

func TestHealthChecker_Start_NilHealthCheck(t *testing.T) {
	hc := NewHealthChecker()
	v := &VM{
		ID:    "no-check",
		Cfg:   Config{},
		State: StateRunning,
		done:  make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hc.Start(ctx, v)
}

func TestHealthChecker_Stop_NoProbe(t *testing.T) {
	hc := NewHealthChecker()
	hc.Stop("nonexistent")
}

func TestProbeTarget_HTTPEmptyPath(t *testing.T) {
	v := &VM{Cfg: Config{}}
	cfg := &HealthCheckConfig{Type: "http", Port: 8080}
	got := probeTarget(v, cfg)
	require.Equal(t, "http://127.0.0.1:8080/", got)
}

func TestHealthChecker_Run_ContextCancelled(t *testing.T) {
	hc := NewHealthChecker()
	ctx, cancel := context.WithCancel(context.Background())
	v := &VM{
		ID:    "ctx-cancel",
		Cfg:   Config{PortMaps: []PortMap{{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}}},
		State: StateRunning,
		done:  make(chan struct{}),
	}
	v.Cfg.HealthCheck = &HealthCheckConfig{
		Type:     "tcp",
		Port:     1,
		Interval: 50 * time.Millisecond,
		Timeout:  50 * time.Millisecond,
		Retries:  3,
	}
	hc.Start(ctx, v)
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(200 * time.Millisecond)
}
