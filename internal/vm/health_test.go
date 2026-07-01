//go:build linux

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

func TestHealthChecker_TCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	v := &VM{
		ID:    "test-health",
		Cfg:   Config{PortMaps: []PortMap{{HostPort: uint16(port), GuestPort: 80, Protocol: ProtocolTCP}}},
		State: StateRunning,
		done:  make(chan struct{}),
	}

	cfg := &HealthCheckConfig{Type: "tcp", Port: port}
	target := probeTarget(v, cfg)
	result := probeTCP(context.Background(), target)
	require.True(t, result)
}

func TestHealthChecker_TCPProbe_Fails(t *testing.T) {
	result := probeTCP(context.Background(), "127.0.0.1:1")
	require.False(t, result)
}

func TestHealthChecker_HTTPProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	result := probeHTTP(context.Background(), srv.URL+"/health")
	require.True(t, result)
}

func TestHealthChecker_HTTPProbe_Fails(t *testing.T) {
	result := probeHTTP(context.Background(), "http://127.0.0.1:1/health")
	require.False(t, result)
}

func TestHealthChecker_HTTPProbe_500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	result := probeHTTP(context.Background(), srv.URL+"/")
	require.False(t, result)
}

func TestHealthChecker_ProbeTarget(t *testing.T) {
	t.Run("tcp with port maps", func(t *testing.T) {
		v := &VM{
			Cfg: Config{
				PortMaps: []PortMap{{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}},
			},
		}
		cfg := &HealthCheckConfig{Type: "tcp"}
		got := probeTarget(v, cfg)
		require.Equal(t, "127.0.0.1:8080", got)
	})

	t.Run("http with explicit port and path", func(t *testing.T) {
		v := &VM{Cfg: Config{}}
		cfg := &HealthCheckConfig{Type: "http", Port: 3000, Path: "/health"}
		got := probeTarget(v, cfg)
		require.Equal(t, "http://127.0.0.1:3000/health", got)
	})

	t.Run("no port maps and no port", func(t *testing.T) {
		v := &VM{Cfg: Config{}}
		cfg := &HealthCheckConfig{Type: "tcp"}
		got := probeTarget(v, cfg)
		require.Empty(t, got)
	})
}

func TestHealthChecker_StartStop(t *testing.T) {
	hc := NewHealthChecker()
	v := &VM{
		ID: "test-hc",
		Cfg: Config{
			PortMaps:    []PortMap{{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}},
			HealthCheck: &HealthCheckConfig{Type: "tcp", Port: 8080},
		},
		State: StateRunning,
		done:  make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hc.Start(ctx, v)
	require.Equal(t, HealthStarting, v.GetHealthStatus())
	hc.Stop(v.ID)
}

func TestHealthChecker_Stop_Idempotent(t *testing.T) {
	hc := NewHealthChecker()
	v := &VM{
		ID: "test-hc-idem",
		Cfg: Config{
			PortMaps:    []PortMap{{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}},
			HealthCheck: &HealthCheckConfig{Type: "tcp", Port: 8080},
		},
		State: StateRunning,
		done:  make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hc.Start(ctx, v)
	// Stopping twice (as the stop path and then the remove path do) must not
	// panic with "close of closed channel".
	require.NotPanics(t, func() {
		hc.Stop(v.ID)
		hc.Stop(v.ID)
	})
}

func TestProbeTargetAddsSlash(t *testing.T) {
	v := &VM{Cfg: Config{}}
	cfg := &HealthCheckConfig{Type: "http", Port: 8080, Path: "health"}
	got := probeTarget(v, cfg)
	require.Equal(t, "http://127.0.0.1:8080/health", got)
}

func TestProbeHTTP_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := probeHTTP(ctx, "http://127.0.0.1:99999/health")
	require.False(t, result)
}

func TestProbeTCP_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := probeTCP(ctx, "127.0.0.1:1")
	require.False(t, result)
}

func TestHealthDefaults(t *testing.T) {
	cfg := HealthCheckConfig{}
	require.Equal(t, time.Duration(0), cfg.Interval)
	require.Equal(t, time.Duration(0), cfg.Timeout)
	require.Equal(t, 0, cfg.Retries)
}
