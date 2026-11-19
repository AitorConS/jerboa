//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Health checks against targets that never answer, answer slowly, answer with
// errors, or flap. A probe must be bounded by its timeout, one bad answer must
// not flip a healthy VM, and a stopped probe must stop.

// closedPort returns a loopback address that refuses connections: a socket
// bound but not listening, so no other process can take the port meanwhile.
func closedPort(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Close(fd) })
	require.NoError(t, syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}))
	sa, err := syscall.Getsockname(fd)
	require.NoError(t, err)
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(sa.(*syscall.SockaddrInet4).Port))
}

// silentListener accepts connections and never writes a byte, like a guest
// whose service is deadlocked.
func silentListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

func newProbe(v *VM, typ, target string, retries int) *healthProbe {
	return &healthProbe{
		vm:     v,
		cfg:    HealthCheckConfig{Type: typ, Timeout: 300 * time.Millisecond, Retries: retries, Interval: time.Hour},
		target: target,
		done:   make(chan struct{}),
	}
}

func startingVM() *VM {
	return &VM{ID: "probe", HealthStatus: HealthStarting, done: make(chan struct{})}
}

func TestFaults_HealthTargetThatNeverAnswers(t *testing.T) {
	targets := map[string]struct{ typ, target string }{
		"tcp refused":  {"tcp", closedPort(t)},
		"http refused": {"http", "http://" + closedPort(t) + "/"},
		"http silent":  {"http", "http://" + silentListener(t) + "/"},
	}
	for name, tc := range targets {
		t.Run(name, func(t *testing.T) {
			h := NewHealthChecker()
			v := startingVM()
			p := newProbe(v, tc.typ, tc.target, 3)

			for attempt := 1; attempt <= 2; attempt++ {
				begin := time.Now()
				h.probe(p)
				assert.Less(t, time.Since(begin), 5*time.Second, "a probe outlived its timeout")
				assert.Equal(t, HealthStarting, v.GetHealthStatus(),
					"failure %d of 3 already declared the VM unhealthy", attempt)
			}
			h.probe(p)
			assert.Equal(t, HealthUnhealthy, v.GetHealthStatus())
			h.probe(p)
			assert.Equal(t, HealthUnhealthy, v.GetHealthStatus())
		})
	}
}

func TestFaults_HealthFlapping(t *testing.T) {
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if up.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	h := NewHealthChecker()
	v := startingVM()
	p := newProbe(v, "http", srv.URL+"/health", 3)
	step := func(serviceUp bool, want HealthStatus, why string) {
		t.Helper()
		up.Store(serviceUp)
		h.probe(p)
		assert.Equal(t, want, v.GetHealthStatus(), why)
	}

	step(true, HealthHealthy, "first good answer")
	step(false, HealthHealthy, "one failure must not flip a healthy VM")
	step(false, HealthHealthy, "two failures must not flip a healthy VM")
	step(true, HealthHealthy, "recovered")
	// The earlier failures were forgiven: it takes three in a row again.
	step(false, HealthHealthy, "the failure count was not reset by a success")
	step(false, HealthHealthy, "the failure count was not reset by a success")
	step(false, HealthUnhealthy, "three failures in a row")
	step(false, HealthUnhealthy, "still down")
	step(true, HealthHealthy, "one good answer is enough to be healthy again")
}

func TestFaults_HealthHTTPAnswers(t *testing.T) {
	var elsewhereHits atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhereHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(elsewhere.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, elsewhere.URL, http.StatusFound)
		case "/hang-up":
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_ = c.Close()
		case "/garbage":
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_, _ = c.Write([]byte("\x00\x01 this is not HTTP\r\n\r\n"))
			_ = c.Close()
		default:
			code, _ := strconv.Atoi(r.URL.Path[1:])
			w.WriteHeader(code)
		}
	}))
	t.Cleanup(srv.Close)

	for path, want := range map[string]HealthStatus{
		"/200":     HealthHealthy,
		"/204":     HealthHealthy,
		"/399":     HealthHealthy,
		"/400":     HealthUnhealthy,
		"/404":     HealthUnhealthy,
		"/500":     HealthUnhealthy,
		"/503":     HealthUnhealthy,
		"/hang-up": HealthUnhealthy,
		"/garbage": HealthUnhealthy,
		// A redirect is the answer; it is not followed.
		"/redirect": HealthHealthy,
	} {
		t.Run(path, func(t *testing.T) {
			h := NewHealthChecker()
			v := startingVM()
			h.probe(newProbe(v, "http", srv.URL+path, 1))
			assert.Equal(t, want, v.GetHealthStatus())
		})
	}
	assert.Zero(t, elsewhereHits.Load(), "the daemon followed a redirect issued by a guest")
}

func TestFaults_HealthThroughTheGuestNetworkDialer(t *testing.T) {
	t.Run("dial fails", func(t *testing.T) {
		for _, typ := range []string{"tcp", "http"} {
			h := NewHealthChecker()
			v := startingVM()
			var dials atomic.Int32
			v.healthDial = func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				return nil, errors.New("no route to guest")
			}
			target := "10.0.2.15:80"
			if typ == "http" {
				target = "http://10.0.2.15:80/"
			}
			h.probe(newProbe(v, typ, target, 1))
			assert.Equal(t, HealthUnhealthy, v.GetHealthStatus(), typ)
			assert.Positive(t, dials.Load(), "the probe bypassed the guest network")
		}
	})
	t.Run("dial hangs until the timeout", func(t *testing.T) {
		h := NewHealthChecker()
		v := startingVM()
		v.healthDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done() // a guest that never completes the handshake
			return nil, ctx.Err()
		}
		for _, typ := range []string{"tcp", "http"} {
			target := "10.0.2.15:80"
			if typ == "http" {
				target = "http://10.0.2.15:80/"
			}
			begin := time.Now()
			h.probe(newProbe(v, typ, target, 1))
			assert.Less(t, time.Since(begin), 5*time.Second, "%s probe was not bounded by its timeout", typ)
			assert.Equal(t, HealthUnhealthy, v.GetHealthStatus())
		}
	})
	t.Run("connection is closed after a tcp probe", func(t *testing.T) {
		h := NewHealthChecker()
		v := startingVM()
		client, server := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		v.healthDial = func(context.Context, string, string) (net.Conn, error) { return client, nil }
		h.probe(newProbe(v, "tcp", "10.0.2.15:80", 1))
		assert.Equal(t, HealthHealthy, v.GetHealthStatus())
		_ = server.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err := server.Read(make([]byte, 1))
		require.Error(t, err)
		var nerr net.Error
		assert.False(t, errors.As(err, &nerr) && nerr.Timeout(), "the probe leaked its connection to the guest")
	})
}

func TestFaults_HealthUnknownProbeType(t *testing.T) {
	h := NewHealthChecker()
	v := startingVM()
	h.probe(newProbe(v, "icmp", "127.0.0.1:1", 1))
	assert.Equal(t, HealthUnknown, v.GetHealthStatus(), "an unsupported check must not report healthy or unhealthy")
}

func TestFaults_HealthLoopDeclaresUnhealthyAndStops(t *testing.T) {
	host, port, err := net.SplitHostPort(closedPort(t))
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host)
	portNum, err := strconv.Atoi(port)
	require.NoError(t, err)

	h := NewHealthChecker()
	v := &VM{ID: "loop", done: make(chan struct{}), Cfg: Config{
		HealthCheck: &HealthCheckConfig{Type: "tcp", Port: portNum, Interval: 5 * time.Millisecond, Timeout: 200 * time.Millisecond, Retries: 2},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Start(ctx, v)
	require.Equal(t, 1, probeCountFor(h))
	assert.Equal(t, HealthStarting, v.GetHealthStatus(), "a VM is not judged before its first probe")
	require.Eventually(t, func() bool { return v.GetHealthStatus() == HealthUnhealthy }, 30*time.Second, 5*time.Millisecond)

	// Stop is safe to repeat and from several callers (stop path, remove
	// path, monitor), and for VMs that have no probe.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); h.Stop(v.ID) }()
	}
	wg.Wait()
	h.Stop("never-started")
	assert.Zero(t, probeCountFor(h))

	// After Stop the probe no longer writes: a status set now stays. One
	// probe may have been in flight when Stop was called, so the status is
	// only required to become stable.
	require.Eventually(t, func() bool {
		v.SetHealthStatus(HealthUnknown)
		time.Sleep(100 * time.Millisecond) // twenty probe intervals
		return v.GetHealthStatus() == HealthUnknown
	}, 30*time.Second, time.Millisecond, "a stopped probe is still running")
}

// A record written before the values were validated can hold a negative
// interval, timeout or retry count. Starting its probe must not panic (a
// panic there ends the daemon, and it adopts the same record at every start):
// the defaults apply instead.
func TestFaults_HealthStoredNegativeValuesUseTheDefaults(t *testing.T) {
	_, port, err := net.SplitHostPort(closedPort(t))
	require.NoError(t, err)
	portNum, err := strconv.Atoi(port)
	require.NoError(t, err)

	h := NewHealthChecker()
	v := &VM{ID: "negative", done: make(chan struct{}), Cfg: Config{
		HealthCheck: &HealthCheckConfig{Type: "tcp", Port: portNum, Interval: -time.Second, Timeout: -time.Second, Retries: -1},
	}}
	h.Start(context.Background(), v)
	t.Cleanup(func() { h.Stop(v.ID) })

	h.mu.Lock()
	p := h.probes[v.ID]
	h.mu.Unlock()
	require.NotNil(t, p)
	assert.Equal(t, defaultHealthInterval, p.cfg.Interval)
	assert.Equal(t, defaultHealthTimeout, p.cfg.Timeout)
	assert.Equal(t, defaultHealthRetries, p.cfg.Retries)
	assert.Equal(t, -time.Second, v.Cfg.HealthCheck.Interval, "the VM's own configuration is not rewritten")
}

func TestFaults_HealthLoopEndsWithItsContext(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	portNum, err := strconv.Atoi(port)
	require.NoError(t, err)

	h := NewHealthChecker()
	v := &VM{ID: "ctx", done: make(chan struct{}), Cfg: Config{
		HealthCheck: &HealthCheckConfig{Type: "http", Port: portNum, Interval: 5 * time.Millisecond},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx, v)
	require.Eventually(t, func() bool { return v.GetHealthStatus() == HealthHealthy }, 30*time.Second, 5*time.Millisecond)
	// The daemon shuts down without stopping each probe.
	cancel()
	require.Eventually(t, func() bool {
		before := hits.Load()
		time.Sleep(50 * time.Millisecond)
		return hits.Load() == before
	}, 30*time.Second, time.Millisecond, "the probe kept running after its context ended")
}

func TestFaults_HealthCheckWithNothingToProbe(t *testing.T) {
	h := NewHealthChecker()
	// No health check configured, and one with no port to derive a target.
	for _, cfg := range []Config{{}, {HealthCheck: &HealthCheckConfig{Type: "tcp"}}} {
		v := &VM{ID: "none", done: make(chan struct{}), Cfg: cfg}
		h.Start(context.Background(), v)
		assert.Zero(t, probeCountFor(h))
		assert.Empty(t, v.GetHealthStatus(), "a VM without a usable check must not be shown as starting forever")
	}
}

func TestFaults_ProbeTargetPrefersTheGuestAddress(t *testing.T) {
	ports := []PortMap{{HostPort: 18080, GuestPort: 8080}}
	cases := []struct {
		name string
		v    *VM
		cfg  HealthCheckConfig
		want string
	}{
		{"guest ip and check port", &VM{Cfg: Config{IPAddress: "10.1.0.5", PortMaps: ports}}, HealthCheckConfig{Type: "tcp", Port: 9000}, "10.1.0.5:9000"},
		{"guest ip, port from mapping", &VM{Cfg: Config{IPAddress: "10.1.0.5", PortMaps: ports}}, HealthCheckConfig{Type: "tcp"}, "10.1.0.5:8080"},
		{"no guest ip, host port", &VM{Cfg: Config{PortMaps: ports}}, HealthCheckConfig{Type: "tcp"}, "127.0.0.1:18080"},
		{"no guest ip, check port", &VM{Cfg: Config{}}, HealthCheckConfig{Type: "tcp", Port: 9000}, "127.0.0.1:9000"},
		{"native address wins", &VM{healthAddress: "10.0.2.15", Cfg: Config{IPAddress: "10.1.0.5", PortMaps: ports}}, HealthCheckConfig{Type: "tcp"}, "10.0.2.15:8080"},
		{"native address without any port", &VM{healthAddress: "10.0.2.15"}, HealthCheckConfig{Type: "tcp"}, ""},
		{"nothing to probe", &VM{}, HealthCheckConfig{Type: "tcp"}, ""},
		{"http path is normalized", &VM{Cfg: Config{IPAddress: "10.1.0.5"}}, HealthCheckConfig{Type: "http", Port: 80, Path: "healthz"}, "http://10.1.0.5:80/healthz"},
		{"http default path", &VM{Cfg: Config{IPAddress: "10.1.0.5"}}, HealthCheckConfig{Type: "http", Port: 80}, "http://10.1.0.5:80/"},
		{"ipv6 guest", &VM{Cfg: Config{IPAddress: "fd00::5"}}, HealthCheckConfig{Type: "http", Port: 80, Path: "/h"}, "http://[fd00::5]:80/h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, probeTarget(tc.v, &tc.cfg))
		})
	}
}
