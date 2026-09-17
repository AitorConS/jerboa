package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// measureFileSizes fills logical, allocated and compressed sizes for path.
// Compressed sizes approximate a registry download: gzip -6 (Go default) and
// zstd -19 when the zstd CLI is installed.
func measureFileSizes(ctx context.Context, path string, info *imageInfo) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.LogicalBytes == 0 {
		info.LogicalBytes = st.Size()
	}
	info.AllocatedBytes = allocatedBytes(st)

	f, err := os.Open(path) //nolint:gosec // benchmark artifact
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var counter countingWriter
	gz := gzip.NewWriter(&counter)
	if _, err := io.Copy(gz, f); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	info.GzipBytes = counter.n

	if _, err := exec.LookPath("zstd"); err == nil {
		cmd := exec.CommandContext(ctx, "zstd", "-19", "-T0", "-q", "-c", path)
		var zc countingWriter
		cmd.Stdout = &zc
		if err := cmd.Run(); err == nil {
			info.ZstdBytes = zc.n
		}
	}
	return nil
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// readyProbe is "http:/path" (any 2xx) or "tcp".
type readyProbe struct {
	kind string
	path string
}

func parseReadyProbe(s string) (readyProbe, error) {
	switch {
	case s == "tcp":
		return readyProbe{kind: "tcp"}, nil
	case strings.HasPrefix(s, "http:/"):
		return readyProbe{kind: "http", path: strings.TrimPrefix(s, "http:")}, nil
	default:
		return readyProbe{}, fmt.Errorf("ready probe %q: want http:/path or tcp", s)
	}
}

// waitReady polls addr until the probe succeeds or ctx expires. A published
// port can accept TCP before the guest service listens (the userspace
// forwarder accepts first), so HTTP probes are the meaningful readiness signal.
func waitReady(ctx context.Context, addr string, probe readyProbe) error {
	client := &http.Client{Timeout: 250 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	for {
		var ok bool
		switch probe.kind {
		case "tcp":
			conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				ok = true
			}
		default:
			resp, err := client.Get("http://" + addr + probe.path)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				ok = resp.StatusCode >= 200 && resp.StatusCode < 300
			}
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready at %s: %w", addr, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// loadResult is one load-test measurement.
type loadResult struct {
	Target      string  `json:"target"`
	Requests    int64   `json:"requests"`
	Errors      int64   `json:"errors"`
	RPS         float64 `json:"rps"`
	LatencyP50  float64 `json:"latency_p50_ms"`
	LatencyP90  float64 `json:"latency_p90_ms"`
	LatencyP99  float64 `json:"latency_p99_ms"`
	ExternalTPS float64 `json:"external_metric,omitempty"`
	Output      string  `json:"output,omitempty"`
}

// httpLoad runs a closed-loop HTTP load test with keep-alive connections.
func httpLoad(ctx context.Context, url string, concurrency int, duration time.Duration) loadResult {
	transport := &http.Transport{MaxIdleConnsPerHost: concurrency, MaxConnsPerHost: concurrency}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	var (
		mu        sync.Mutex
		latencies []float64
		errs      atomic.Int64
		wg        sync.WaitGroup
	)
	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]float64, 0, 4096)
			for ctx.Err() == nil {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				t0 := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						errs.Add(1)
					}
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errs.Add(1)
					continue
				}
				local = append(local, float64(time.Since(t0).Microseconds())/1000)
			}
			mu.Lock()
			latencies = append(latencies, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	sort.Float64s(latencies)
	return loadResult{
		Target:     url,
		Requests:   int64(len(latencies)),
		Errors:     errs.Load(),
		RPS:        float64(len(latencies)) / elapsed,
		LatencyP50: percentile(latencies, 50),
		LatencyP90: percentile(latencies, 90),
		LatencyP99: percentile(latencies, 99),
	}
}

// externalLoad runs a user command (e.g. pgbench) with {host} and {port}
// substituted and extracts the first capture group of metricRe from its
// output (e.g. `tps = ([0-9.]+)`).
func externalLoad(ctx context.Context, template, host string, port int, metricRe *regexp.Regexp) loadResult {
	expanded := strings.NewReplacer("{host}", host, "{port}", strconv.Itoa(port)).Replace(template)
	res := loadResult{Target: expanded}
	cmd := exec.CommandContext(ctx, "sh", "-c", expanded)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	res.Output = tail(out.String(), 2000)
	if err != nil {
		res.Errors = 1
		return res
	}
	if m := metricRe.FindStringSubmatch(out.String()); len(m) > 1 {
		res.ExternalTPS, _ = strconv.ParseFloat(m[1], 64)
	}
	return res
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// freePort asks the kernel for an unused loopback port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}
