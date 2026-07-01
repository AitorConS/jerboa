//go:build linux

package vm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultHealthInterval = 10 * time.Second
	defaultHealthTimeout  = 3 * time.Second
	defaultHealthRetries  = 3
)

type healthProbe struct {
	vm       *VM
	cfg      HealthCheckConfig
	target   string
	failures int
	done     chan struct{}
}

type HealthChecker struct {
	mu     sync.Mutex
	probes map[string]*healthProbe
}

func NewHealthChecker() *HealthChecker {
	return &HealthChecker{probes: make(map[string]*healthProbe)}
}

func (h *HealthChecker) Start(ctx context.Context, v *VM) {
	if v.Cfg.HealthCheck == nil {
		return
	}
	cfg := *v.Cfg.HealthCheck
	if cfg.Interval == 0 {
		cfg.Interval = defaultHealthInterval
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultHealthTimeout
	}
	if cfg.Retries == 0 {
		cfg.Retries = defaultHealthRetries
	}

	target := probeTarget(v, &cfg)
	if target == "" {
		return
	}

	p := &healthProbe{
		vm:     v,
		cfg:    cfg,
		target: target,
		done:   make(chan struct{}),
	}

	h.mu.Lock()
	h.probes[v.ID] = p
	h.mu.Unlock()

	v.SetHealthStatus(HealthStarting)
	go h.run(ctx, p)
}

func (h *HealthChecker) Stop(id string) {
	// Delete the probe under the lock so Stop is idempotent: a second call (e.g.
	// the stop path and then the remove path both stopping the same VM's probe)
	// finds nothing and does not close an already-closed channel — which would
	// panic and, before recovery was added, crash the daemon.
	h.mu.Lock()
	p, ok := h.probes[id]
	if ok {
		delete(h.probes, id)
	}
	h.mu.Unlock()
	if ok && p.done != nil {
		close(p.done)
	}
}

func (h *HealthChecker) run(ctx context.Context, p *healthProbe) {
	grace := time.After(p.cfg.Interval)
	select {
	case <-grace:
	case <-p.done:
		return
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	for {
		h.probe(p)
		select {
		case <-ticker.C:
		case <-p.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (h *HealthChecker) probe(p *healthProbe) {
	probeCtx, cancel := context.WithTimeout(context.Background(), p.cfg.Timeout)
	defer cancel()

	var ok bool
	switch p.cfg.Type {
	case "tcp":
		ok = probeTCP(probeCtx, p.target)
	case "http":
		ok = probeHTTP(probeCtx, p.target)
	default:
		p.vm.SetHealthStatus(HealthUnknown)
		return
	}

	if ok {
		p.failures = 0
		p.vm.SetHealthStatus(HealthHealthy)
	} else {
		p.failures++
		if p.failures >= p.cfg.Retries {
			p.vm.SetHealthStatus(HealthUnhealthy)
		}
	}
}

func probeTarget(v *VM, cfg *HealthCheckConfig) string {
	if len(v.Cfg.PortMaps) == 0 && cfg.Port == 0 {
		return ""
	}
	hostPort := cfg.Port
	if hostPort == 0 && len(v.Cfg.PortMaps) > 0 {
		hostPort = int(v.Cfg.PortMaps[0].HostPort)
	}
	if hostPort == 0 {
		return ""
	}
	if cfg.Type == "http" {
		path := cfg.Path
		if path != "" && !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		if path == "" {
			path = "/"
		}
		return fmt.Sprintf("http://127.0.0.1:%d%s", hostPort, path)
	}
	return fmt.Sprintf("127.0.0.1:%d", hostPort)
}

func probeTCP(ctx context.Context, addr string) bool {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func probeHTTP(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}
