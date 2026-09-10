//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"net"
	"net/http"
	"strconv"
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

	p.vm.mu.RLock()
	dial := p.vm.healthDial
	p.vm.mu.RUnlock()
	var ok bool
	if dial != nil {
		if p.cfg.Type == "tcp" {
			c, err := dial(probeCtx, "tcp", p.target)
			ok = err == nil
			if c != nil {
				c.Close()
			}
		} else if p.cfg.Type == "http" {
			transport := &http.Transport{DialContext: dial}
			defer transport.CloseIdleConnections()
			req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, p.target, nil)
			if err == nil {
				resp, err := (&http.Client{Transport: transport}).Do(req)
				if err == nil {
					ok = resp.StatusCode >= 200 && resp.StatusCode < 400
					resp.Body.Close()
				}
			}
		}
	} else {
		switch p.cfg.Type {
		case "tcp":
			ok = probeTCP(probeCtx, p.target)
		case "http":
			ok = probeHTTP(probeCtx, p.target)
		default:
			p.vm.SetHealthStatus(HealthUnknown)
			return
		}

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

// probeTarget builds the address the health checker dials for v. The health
// check port (cfg.Port) is the GUEST service port, so when the VM has a routable
// guest IP the probe goes straight to guestIP:guestPort — the address the
// service actually listens on, reachable from the daemon over the bridge. This
// is independent of how (or whether) the port is published on the host.
//
// Dialing the host-published loopback port instead (the previous behavior) was
// wrong in two ways: a VM published with host≠guest (e.g. -p 18080:8080,
// --health-check tcp:8080) probed 127.0.0.1:8080 where nothing of that VM
// listened → permanently unhealthy; and when two VMs collided on a host port the
// probe reached whichever VM won the bind, not the one being checked.
//
// Only when the VM has no guest IP (no TAP networking) does it fall back to the
// host loopback, mapping the guest port to its published host port when one
// exists, so a purely local port can still be checked.
func probeTarget(v *VM, cfg *HealthCheckConfig) string {
	v.mu.RLock()
	nativeIP := v.healthAddress
	v.mu.RUnlock()
	if nativeIP != "" {
		port := cfg.Port
		if port == 0 && len(v.Cfg.PortMaps) > 0 {
			port = int(v.Cfg.PortMaps[0].GuestPort)
		}
		if port == 0 {
			return ""
		}
		return formatProbeTarget(cfg, nativeIP, port)
	}
	if len(v.Cfg.PortMaps) == 0 && cfg.Port == 0 {
		return ""
	}
	if v.Cfg.IPAddress != "" {
		guestPort := cfg.Port
		if guestPort == 0 && len(v.Cfg.PortMaps) > 0 {
			guestPort = int(v.Cfg.PortMaps[0].GuestPort)
		}
		if guestPort != 0 {
			return formatProbeTarget(cfg, v.Cfg.IPAddress, guestPort)
		}
	}
	// No routable guest IP: fall back to the host side. cfg.Port names the
	// loopback port to dial; when unset, use the first port map's host port.
	hostPort := cfg.Port
	if hostPort == 0 && len(v.Cfg.PortMaps) > 0 {
		hostPort = int(v.Cfg.PortMaps[0].HostPort)
	}
	if hostPort == 0 {
		return ""
	}
	return formatProbeTarget(cfg, "127.0.0.1", hostPort)
}

// formatProbeTarget renders host+port as the probe address for cfg's type: a
// bare host:port for tcp, or an http URL (with a normalized path) for http.
func formatProbeTarget(cfg *HealthCheckConfig, host string, port int) string {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if cfg.Type == "http" {
		path := cfg.Path
		if path == "" {
			path = "/"
		} else if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		return "http://" + addr + path
	}
	return addr
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
