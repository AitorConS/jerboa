// Command jerboa-bench compares Jerboa hypervisor backends and Docker on the
// same application binary with repeated, randomized, phase-separated runs.
//
// Every runtime boots the same static binary (built once and verified by
// SHA-256 inside each image). Runs are interleaved in a seeded random order
// after warmups, and each run records create (CLI returns), ready (first
// successful probe through the published port), stop and remove separately,
// plus VMM/container memory and optional load results. Image logical,
// allocated and compressed sizes are reported separately.
//
// Example:
//
//	jerboa-bench \
//	  --runtime qemu=jerboa,host=unix:///tmp/q.sock,network=bench \
//	  --runtime fc=jerboa,host=unix:///tmp/fc.sock,network=bench,layout=compact \
//	  --runtime docker=docker \
//	  --runs 30 --warmup 3 --load-duration 10s --out bench.json
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

type runtimeFlags []string

func (r *runtimeFlags) String() string     { return strings.Join(*r, " ") }
func (r *runtimeFlags) Set(v string) error { *r = append(*r, v); return nil }

type options struct {
	runtimes     runtimeFlags
	jerboaBin    string
	appBinary    string
	appArch      string
	guestPort    int
	memory       string
	runs         int
	warmup       int
	seed         int64
	ready        string
	readyTimeout time.Duration
	idleSample   time.Duration
	loadDuration time.Duration
	loadConc     int
	loadPath     string
	loadDirect   bool
	loadCmd      string
	loadMetric   string
	out          string
	csvOut       string
	keepGoing    bool
}

// slot is one scheduled run.
type slot struct {
	Runtime   string
	Iteration int
	Warmup    bool
}

type runRecord struct {
	Runtime       string       `json:"runtime"`
	Iteration     int          `json:"iteration"`
	Order         int          `json:"order"`
	Warmup        bool         `json:"warmup"`
	CreateMs      float64      `json:"create_ms"`
	ReadyMs       float64      `json:"ready_ms"`
	StopMs        float64      `json:"stop_ms"`
	StoppedMs     float64      `json:"stopped_ms"`
	RemoveMs      float64      `json:"remove_ms"`
	MemReadyBytes int64        `json:"mem_ready_bytes"`
	MemIdleBytes  int64        `json:"mem_idle_bytes,omitempty"`
	MemSource     string       `json:"mem_source,omitempty"`
	Load          []loadResult `json:"load,omitempty"`
	Error         string       `json:"error,omitempty"`
}

type report struct {
	Meta      map[string]any                `json:"meta"`
	Runtimes  []runtimeSpec                 `json:"runtimes"`
	Images    map[string]imageInfo          `json:"images"`
	Summaries map[string]map[string]summary `json:"summaries"`
	Runs      []runRecord                   `json:"runs"`
}

func main() {
	var o options
	flag.Var(&o.runtimes, "runtime", "runtime to benchmark, label=jerboa|docker[,host=...,layout=...,network=...,store=...,image=...,args=...] (repeatable)")
	flag.StringVar(&o.jerboaBin, "jerboa", "jerboa", "jerboa CLI binary")
	flag.StringVar(&o.appBinary, "app-binary", "", "static Linux binary to benchmark (default: build cmd/jerboa-bench/testapp)")
	flag.StringVar(&o.appArch, "app-arch", runtime.GOARCH, "GOARCH for the default test app")
	flag.IntVar(&o.guestPort, "guest-port", 8080, "port the application listens on inside the guest/container")
	flag.StringVar(&o.memory, "memory", "128M", "guest memory baked into built Jerboa images")
	flag.IntVar(&o.runs, "runs", 30, "measured runs per runtime")
	flag.IntVar(&o.warmup, "warmup", 3, "discarded warmup runs per runtime")
	flag.Int64Var(&o.seed, "seed", 1, "seed for run order and bootstrap confidence intervals")
	flag.StringVar(&o.ready, "ready", "http:/ready", "readiness probe through the published port: http:/path or tcp")
	flag.DurationVar(&o.readyTimeout, "ready-timeout", 60*time.Second, "maximum time to wait for readiness")
	flag.DurationVar(&o.idleSample, "idle-sample", 0, "also sample memory after this idle period (0 disables)")
	flag.DurationVar(&o.loadDuration, "load-duration", 0, "HTTP load test duration per run (0 disables)")
	flag.IntVar(&o.loadConc, "load-concurrency", 16, "concurrent HTTP connections for the load test")
	flag.StringVar(&o.loadPath, "load-path", "/", "HTTP path requested by the load test")
	flag.BoolVar(&o.loadDirect, "load-direct", false, "also load the guest/container IP directly to isolate published-port overhead (Linux)")
	flag.StringVar(&o.loadCmd, "load-cmd", "", "external load command instead of the HTTP load test; {host} and {port} are substituted (e.g. pgbench)")
	flag.StringVar(&o.loadMetric, "load-metric", `tps = ([0-9.]+)`, "regexp whose first group is the external load command's metric")
	flag.StringVar(&o.out, "out", "jerboa-bench.json", "JSON report path")
	flag.StringVar(&o.csvOut, "csv", "", "CSV of individual runs (default: <out>.csv)")
	flag.BoolVar(&o.keepGoing, "keep-going", false, "record failed runs and continue instead of aborting")
	flag.Parse()

	if err := run(context.Background(), o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "jerboa-bench:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options, stdout io.Writer) error {
	if len(o.runtimes) == 0 {
		return fmt.Errorf("at least one --runtime is required")
	}
	probe, err := parseReadyProbe(o.ready)
	if err != nil {
		return err
	}
	metricRe, err := regexp.Compile(o.loadMetric)
	if err != nil {
		return fmt.Errorf("--load-metric: %w", err)
	}
	var runtimes []benchRuntime
	byLabel := map[string]benchRuntime{}
	for _, s := range o.runtimes {
		spec, err := parseRuntimeSpec(s)
		if err != nil {
			return err
		}
		if _, dup := byLabel[spec.Label]; dup {
			return fmt.Errorf("duplicate runtime label %q", spec.Label)
		}
		if spec.Kind == "jerboa" && runtime.GOOS == "linux" && spec.Opts["network"] == "" {
			return fmt.Errorf("runtime %q: Linux port publishing needs network=<name> (create it with `jerboa network create`)", spec.Label)
		}
		rt := newRuntime(spec, o.jerboaBin)
		runtimes = append(runtimes, rt)
		byLabel[spec.Label] = rt
	}

	app, cleanup, err := prepareApp(ctx, o)
	if err != nil {
		return err
	}
	defer cleanup()

	rep := report{
		Meta:      hostMeta(ctx, o, app),
		Images:    map[string]imageInfo{},
		Summaries: map[string]map[string]summary{},
	}
	versions := map[string]string{}
	for _, rt := range runtimes {
		spec := rt.Spec()
		rep.Runtimes = append(rep.Runtimes, spec)
		fmt.Fprintf(os.Stderr, "preparing %s image...\n", spec.Label)
		info, err := rt.Prepare(ctx, app)
		if err != nil {
			return fmt.Errorf("prepare %s: %w", spec.Label, err)
		}
		rep.Images[spec.Label] = info
		for k, v := range rt.Versions(ctx) {
			versions[k] = v
		}
	}
	rep.Meta["versions"] = versions

	plan := schedule(labels(runtimes), o.runs, o.warmup, o.seed)
	for i, s := range plan {
		rec := runOnce(ctx, o, byLabel[s.Runtime], s, i, probe, metricRe)
		fmt.Fprintf(os.Stderr, "[%d/%d] %-10s warmup=%-5v create=%7.1fms ready=%7.1fms stop=%7.1fms stopped=%7.1fms rm=%7.1fms mem=%s %s\n",
			i+1, len(plan), s.Runtime, s.Warmup, rec.CreateMs, rec.ReadyMs, rec.StopMs, rec.StoppedMs, rec.RemoveMs, mib(rec.MemReadyBytes), rec.Error)
		rep.Runs = append(rep.Runs, rec)
		if rec.Error != "" && !o.keepGoing {
			_ = writeReport(o, rep)
			return fmt.Errorf("run %d (%s): %s", i+1, s.Runtime, rec.Error)
		}
	}
	for _, rt := range runtimes {
		label := rt.Spec().Label
		rep.Summaries[label] = summarizeRuntime(rep.Runs, label, o.seed)
	}
	if err := writeReport(o, rep); err != nil {
		return err
	}
	printSummary(stdout, rep, labels(runtimes))
	return nil
}

// prepareApp builds (or hashes) the single application binary every runtime uses.
func prepareApp(ctx context.Context, o options) (appConfig, func(), error) {
	app := appConfig{BinaryPath: o.appBinary, GuestPort: o.guestPort, Memory: o.memory}
	cleanup := func() {}
	if app.BinaryPath == "" {
		dir, err := os.MkdirTemp("", "jerboa-bench-app-")
		if err != nil {
			return app, cleanup, err
		}
		cleanup = func() { _ = os.RemoveAll(dir) }
		app.BinaryPath = filepath.Join(dir, "jerboa-bench-app")
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", app.BinaryPath,
			"github.com/AitorConS/jerboa/cmd/jerboa-bench/testapp")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+o.appArch)
		if out, err := cmd.CombinedOutput(); err != nil {
			cleanup()
			return app, func() {}, fmt.Errorf("build test app (run from the jerboa module or pass --app-binary): %w: %s", err, out)
		}
	}
	sum, err := fileSHA256(app.BinaryPath)
	if err != nil {
		cleanup()
		return app, func() {}, err
	}
	app.BinarySHA256 = sum
	return app, cleanup, nil
}

func labels(rts []benchRuntime) []string {
	var out []string
	for _, rt := range rts {
		out = append(out, rt.Spec().Label)
	}
	return out
}

// schedule returns warmups (each round in shuffled runtime order) followed by
// all measured runs shuffled together, so slow drift on the host (thermal,
// page cache, background jobs) does not systematically favor one runtime.
func schedule(runtimes []string, runs, warmup int, seed int64) []slot {
	r := rand.New(rand.NewSource(seed))
	var plan []slot
	for w := 0; w < warmup; w++ {
		round := append([]string(nil), runtimes...)
		r.Shuffle(len(round), func(i, j int) { round[i], round[j] = round[j], round[i] })
		for _, rt := range round {
			plan = append(plan, slot{Runtime: rt, Iteration: w, Warmup: true})
		}
	}
	var measured []slot
	for _, rt := range runtimes {
		for i := 0; i < runs; i++ {
			measured = append(measured, slot{Runtime: rt, Iteration: i})
		}
	}
	r.Shuffle(len(measured), func(i, j int) { measured[i], measured[j] = measured[j], measured[i] })
	return append(plan, measured...)
}

func runOnce(ctx context.Context, o options, rt benchRuntime, s slot, order int, probe readyProbe, metricRe *regexp.Regexp) (rec runRecord) {
	rec = runRecord{Runtime: s.Runtime, Iteration: s.Iteration, Order: order, Warmup: s.Warmup}
	fail := func(stage string, err error) runRecord {
		rec.Error = stage + ": " + err.Error()
		return rec
	}
	port, err := freePort()
	if err != nil {
		return fail("port", err)
	}
	name := fmt.Sprintf("jerboa-bench-%s-%d-%d", sanitize(s.Runtime), order, time.Now().UnixNano()%1e6)

	t0 := time.Now()
	inst, err := rt.Start(ctx, name, port)
	rec.CreateMs = msSince(t0)
	if err != nil {
		return fail("start", err)
	}
	defer func() {
		if rec.Error == "" {
			return
		}
		// Best-effort cleanup of a failed run so later runs start clean.
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = rt.Stop(cctx, inst)
		_ = rt.WaitStopped(cctx, inst)
		_ = rt.Remove(cctx, inst)
	}()

	readyCtx, cancel := context.WithTimeout(ctx, o.readyTimeout)
	err = waitReady(readyCtx, fmt.Sprintf("127.0.0.1:%d", port), probe)
	cancel()
	rec.ReadyMs = msSince(t0)
	if err != nil {
		return fail("ready", err)
	}

	if b, src, err := rt.MemoryBytes(ctx, inst); err == nil {
		rec.MemReadyBytes, rec.MemSource = b, src
	}
	if o.idleSample > 0 {
		time.Sleep(o.idleSample)
		if b, _, err := rt.MemoryBytes(ctx, inst); err == nil {
			rec.MemIdleBytes = b
		}
	}

	if !s.Warmup {
		rec.Load = runLoad(ctx, o, rt, inst, metricRe)
	}

	rec.StopMs, err = timed(func() error { return rt.Stop(ctx, inst) })
	if err != nil {
		return fail("stop", err)
	}
	settleCtx, cancelSettle := context.WithTimeout(ctx, o.readyTimeout)
	rec.StoppedMs, err = timed(func() error { return rt.WaitStopped(settleCtx, inst) })
	cancelSettle()
	if err != nil {
		return fail("wait stopped", err)
	}
	rec.RemoveMs, err = timed(func() error { return rt.Remove(ctx, inst) })
	if err != nil {
		return fail("remove", err)
	}
	return rec
}

func runLoad(ctx context.Context, o options, rt benchRuntime, inst instance, metricRe *regexp.Regexp) []loadResult {
	type target struct {
		host string
		port int
	}
	targets := []target{{"127.0.0.1", inst.HostPort}}
	if o.loadDirect && runtime.GOOS == "linux" {
		if ip, err := rt.GuestIP(ctx, inst); err == nil && ip != "" {
			targets = append(targets, target{ip, o.guestPort})
		}
	}
	var results []loadResult
	for _, t := range targets {
		switch {
		case o.loadCmd != "":
			results = append(results, externalLoad(ctx, o.loadCmd, t.host, t.port, metricRe))
		case o.loadDuration > 0:
			url := fmt.Sprintf("http://%s:%d%s", t.host, t.port, o.loadPath)
			results = append(results, httpLoad(ctx, url, o.loadConc, o.loadDuration))
		}
	}
	return results
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

func summarizeRuntime(runs []runRecord, label string, seed int64) map[string]summary {
	metrics := map[string][]float64{}
	for _, r := range runs {
		if r.Runtime != label || r.Warmup || r.Error != "" {
			continue
		}
		metrics["create_ms"] = append(metrics["create_ms"], r.CreateMs)
		metrics["ready_ms"] = append(metrics["ready_ms"], r.ReadyMs)
		metrics["stop_ms"] = append(metrics["stop_ms"], r.StopMs)
		metrics["stopped_ms"] = append(metrics["stopped_ms"], r.StoppedMs)
		metrics["remove_ms"] = append(metrics["remove_ms"], r.RemoveMs)
		if r.MemReadyBytes > 0 {
			metrics["mem_ready_mib"] = append(metrics["mem_ready_mib"], float64(r.MemReadyBytes)/(1<<20))
		}
		if r.MemIdleBytes > 0 {
			metrics["mem_idle_mib"] = append(metrics["mem_idle_mib"], float64(r.MemIdleBytes)/(1<<20))
		}
		for i, l := range r.Load {
			kind := "published"
			if i > 0 {
				kind = "direct"
			}
			if l.ExternalTPS > 0 {
				metrics["load_"+kind+"_metric"] = append(metrics["load_"+kind+"_metric"], l.ExternalTPS)
			}
			if l.RPS > 0 {
				metrics["load_"+kind+"_rps"] = append(metrics["load_"+kind+"_rps"], l.RPS)
				metrics["load_"+kind+"_p99_ms"] = append(metrics["load_"+kind+"_p99_ms"], l.LatencyP99)
			}
		}
	}
	out := map[string]summary{}
	for k, v := range metrics {
		out[k] = summarize(v, seed)
	}
	return out
}

func hostMeta(ctx context.Context, o options, app appConfig) map[string]any {
	meta := map[string]any{
		"date":          time.Now().UTC().Format(time.RFC3339),
		"host_os":       runtime.GOOS,
		"host_arch":     runtime.GOARCH,
		"host_cpus":     runtime.NumCPU(),
		"seed":          o.seed,
		"runs":          o.runs,
		"warmup":        o.warmup,
		"ready_probe":   o.ready,
		"app_sha256":    app.BinarySHA256,
		"load_duration": o.loadDuration.String(),
		"load_conc":     o.loadConc,
		"load_cmd":      o.loadCmd,
	}
	if out, err := command(ctx, "uname", "-a"); err == nil {
		meta["uname"] = out
	}
	if out, err := command(ctx, "git", "rev-parse", "HEAD"); err == nil {
		meta["git_commit"] = out
	}
	if runtime.GOOS == "darwin" {
		meta["note"] = "Docker Desktop runs containers inside a Linux VM on macOS; compare against Linux hosts for publishable numbers"
	}
	return meta
}

func writeReport(o options, rep report) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.out, data, 0o644); err != nil {
		return err
	}
	csvPath := o.csvOut
	if csvPath == "" {
		csvPath = strings.TrimSuffix(o.out, filepath.Ext(o.out)) + ".csv"
	}
	f, err := os.Create(csvPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"order", "runtime", "iteration", "warmup", "create_ms", "ready_ms", "stop_ms", "stopped_ms", "remove_ms",
		"mem_ready_bytes", "mem_idle_bytes", "load_published_rps", "load_published_p99_ms", "load_direct_rps", "load_direct_p99_ms", "load_metric", "error"})
	for _, r := range rep.Runs {
		row := []string{strconv.Itoa(r.Order), r.Runtime, strconv.Itoa(r.Iteration), strconv.FormatBool(r.Warmup),
			f2(r.CreateMs), f2(r.ReadyMs), f2(r.StopMs), f2(r.StoppedMs), f2(r.RemoveMs),
			strconv.FormatInt(r.MemReadyBytes, 10), strconv.FormatInt(r.MemIdleBytes, 10)}
		var pubRPS, pubP99, dirRPS, dirP99, metric string
		for i, l := range r.Load {
			if i == 0 {
				pubRPS, pubP99 = f2(l.RPS), f2(l.LatencyP99)
			} else {
				dirRPS, dirP99 = f2(l.RPS), f2(l.LatencyP99)
			}
			if l.ExternalTPS > 0 && metric == "" {
				metric = f2(l.ExternalTPS)
			}
		}
		row = append(row, pubRPS, pubP99, dirRPS, dirP99, metric, r.Error)
		_ = w.Write(row)
	}
	w.Flush()
	return w.Error()
}

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func mib(b int64) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20))
}

func printSummary(w io.Writer, rep report, order []string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "IMAGE\tLOGICAL\tALLOCATED\tGZIP\tZSTD\tBINARY VERIFIED")
	for _, label := range order {
		img := rep.Images[label]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%v\n", label, mib(img.LogicalBytes), mib(img.AllocatedBytes), mib(img.GzipBytes), mib(img.ZstdBytes), img.BinarySHA256 != "")
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "RUNTIME\tMETRIC\tN\tMEDIAN\t95% CI\tP90\tP99\tMEAN±SD")
	for _, label := range order {
		metrics := rep.Summaries[label]
		var names []string
		for k := range metrics {
			names = append(names, k)
		}
		sortMetricNames(names)
		for _, k := range names {
			s := metrics[k]
			fmt.Fprintf(tw, "%s\t%s\t%d\t%.1f\t[%.1f, %.1f]\t%.1f\t%.1f\t%.1f±%.1f\n", label, k, s.N, s.Median, s.MedianCI95[0], s.MedianCI95[1], s.P90, s.P99, s.Mean, s.Stddev)
		}
	}
	_ = tw.Flush()
}

// sortMetricNames orders the lifecycle phases first, then everything else.
func sortMetricNames(names []string) {
	rank := map[string]int{"create_ms": 0, "ready_ms": 1, "stop_ms": 2, "stopped_ms": 3, "remove_ms": 4, "mem_ready_mib": 5, "mem_idle_mib": 6}
	key := func(s string) string {
		if r, ok := rank[s]; ok {
			return strconv.Itoa(r) + s
		}
		return "9" + s
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && key(names[j]) < key(names[j-1]); j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
}
