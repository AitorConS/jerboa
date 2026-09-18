package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseRuntimeSpec(t *testing.T) {
	s, err := parseRuntimeSpec("fc=jerboa,host=unix:///tmp/fc.sock,layout=compact,args=--disk-io-engine async")
	require.NoError(t, err)
	require.Equal(t, "fc", s.Label)
	require.Equal(t, "jerboa", s.Kind)
	require.Equal(t, "unix:///tmp/fc.sock", s.Opts["host"])
	require.Equal(t, []string{"--disk-io-engine", "async"}, splitArgs(s.Opts["args"]))

	d, err := parseRuntimeSpec("docker=docker")
	require.NoError(t, err)
	require.Empty(t, d.Opts)

	for _, bad := range []string{"docker", "=jerboa", "x=podman", "d=docker,layout=compact", "q=jerboa,host"} {
		_, err := parseRuntimeSpec(bad)
		require.Error(t, err, bad)
	}
}

func TestScheduleIsSeededAndBalanced(t *testing.T) {
	plan := schedule([]string{"qemu", "fc", "docker"}, 10, 2, 42)
	require.Len(t, plan, 36)
	require.Equal(t, plan, schedule([]string{"qemu", "fc", "docker"}, 10, 2, 42))
	require.NotEqual(t, plan, schedule([]string{"qemu", "fc", "docker"}, 10, 2, 43))

	for i, s := range plan {
		require.Equal(t, i < 6, s.Warmup, "warmups come first")
	}
	counts := map[string]int{}
	for _, s := range plan[6:] {
		counts[s.Runtime]++
	}
	require.Equal(t, map[string]int{"qemu": 10, "fc": 10, "docker": 10}, counts)
}

func TestParseDockerMemUsage(t *testing.T) {
	for in, want := range map[string]int64{
		"12.5MiB / 7.66GiB": 12.5 * (1 << 20),
		"980KiB / 2GiB":     980 << 10,
		"1.5GiB / 8GiB":     3 << 29,
		"512B / 1GiB":       512,
	} {
		got, err := parseDockerMemUsage(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	_, err := parseDockerMemUsage("-- / --")
	require.Error(t, err)
}

func TestWaitReadyAndHTTPLoad(t *testing.T) {
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-ready:
		default:
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	go func() { time.Sleep(50 * time.Millisecond); close(ready) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t0 := time.Now()
	require.NoError(t, waitReady(ctx, addr, readyProbe{kind: "http", path: "/ready"}))
	require.GreaterOrEqual(t, time.Since(t0), 50*time.Millisecond, "a 503 must not count as ready")
	require.NoError(t, waitReady(ctx, addr, readyProbe{kind: "tcp"}))

	res := httpLoad(context.Background(), srv.URL+"/", 4, 200*time.Millisecond)
	require.Positive(t, res.Requests)
	require.Zero(t, res.Errors)
	require.Positive(t, res.RPS)
	require.LessOrEqual(t, res.LatencyP50, res.LatencyP99)

	short, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedAddr := l.Addr().String()
	require.NoError(t, l.Close())
	require.Error(t, waitReady(short, closedAddr, readyProbe{kind: "http", path: "/"}))
}

func TestExternalLoadMetric(t *testing.T) {
	re := regexp.MustCompile(`tps = ([0-9.]+)`)
	res := externalLoad(context.Background(), "echo host={host} port={port}; echo 'tps = 1234.5 (without initial connection time)'", "10.0.0.2", 5432, re)
	require.Zero(t, res.Errors)
	require.InDelta(t, 1234.5, res.ExternalTPS, 1e-9)
	require.Contains(t, res.Output, "host=10.0.0.2 port=5432")

	failed := externalLoad(context.Background(), "exit 3", "h", 1, re)
	require.Equal(t, int64(1), failed.Errors)
}

func TestSummarizeRuntimeSkipsWarmupsAndFailures(t *testing.T) {
	runs := []runRecord{
		{Runtime: "qemu", Warmup: true, ReadyMs: 1000},
		{Runtime: "qemu", ReadyMs: 100, MemReadyBytes: 64 << 20, Load: []loadResult{{RPS: 900, LatencyP99: 3}, {RPS: 1000, LatencyP99: 2}}},
		{Runtime: "qemu", ReadyMs: 200, Error: "ready: timeout"},
		{Runtime: "docker", ReadyMs: 50},
	}
	s := summarizeRuntime(runs, "qemu", 1)
	require.Equal(t, 1, s["ready_ms"].N)
	require.InDelta(t, 100.0, s["ready_ms"].Median, 1e-9)
	require.InDelta(t, 64.0, s["mem_ready_mib"].Median, 1e-9)
	require.InDelta(t, 900.0, s["load_published_rps"].Median, 1e-9)
	require.InDelta(t, 1000.0, s["load_direct_rps"].Median, 1e-9)
}
