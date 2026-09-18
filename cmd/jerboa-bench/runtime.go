package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// runtimeSpec is one --runtime flag: "label=kind[,key=value...]".
type runtimeSpec struct {
	Label string            `json:"label"`
	Kind  string            `json:"kind"` // jerboa | docker
	Opts  map[string]string `json:"options,omitempty"`
}

// parseRuntimeSpec parses "qemu=jerboa,host=unix:///tmp/q.sock,layout=compact".
// Values cannot contain commas; "args" holds space-separated extra run flags.
func parseRuntimeSpec(s string) (runtimeSpec, error) {
	label, rest, ok := strings.Cut(s, "=")
	if !ok || label == "" {
		return runtimeSpec{}, fmt.Errorf("runtime %q: want label=kind[,key=value...]", s)
	}
	parts := strings.Split(rest, ",")
	spec := runtimeSpec{Label: label, Kind: parts[0], Opts: map[string]string{}}
	if spec.Kind != "jerboa" && spec.Kind != "docker" {
		return runtimeSpec{}, fmt.Errorf("runtime %q: kind must be jerboa or docker", s)
	}
	allowed := map[string]bool{"args": true, "image": true}
	if spec.Kind == "jerboa" {
		for _, k := range []string{"host", "layout", "network", "store", "bin"} {
			allowed[k] = true
		}
	}
	for _, kv := range parts[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !allowed[k] {
			return runtimeSpec{}, fmt.Errorf("runtime %q: unknown or malformed option %q", s, kv)
		}
		spec.Opts[k] = v
	}
	return spec, nil
}

// imageInfo describes the image a runtime boots.
type imageInfo struct {
	Ref string `json:"ref"`
	// BinarySHA256 is the hash of the application binary inside the image as
	// verified by the harness ("" when a prebuilt image is used).
	BinarySHA256    string `json:"binary_sha256,omitempty"`
	LogicalBytes    int64  `json:"logical_bytes,omitempty"`
	AllocatedBytes  int64  `json:"allocated_bytes,omitempty"`
	GzipBytes       int64  `json:"gzip_bytes,omitempty"`
	ZstdBytes       int64  `json:"zstd_bytes,omitempty"`
	CompressionNote string `json:"compression_note,omitempty"`
	Layout          string `json:"layout,omitempty"`
}

// instance is one started VM or container.
type instance struct {
	ID       string
	HostPort int
}

// benchRuntime starts and stops instances of one image.
type benchRuntime interface {
	Spec() runtimeSpec
	Prepare(ctx context.Context, app appConfig) (imageInfo, error)
	Start(ctx context.Context, name string, hostPort int) (instance, error)
	MemoryBytes(ctx context.Context, inst instance) (int64, string, error)
	GuestIP(ctx context.Context, inst instance) (string, error)
	Stop(ctx context.Context, inst instance) error
	// WaitStopped blocks until the instance has fully reached its stopped
	// state, which can lag the Stop call returning.
	WaitStopped(ctx context.Context, inst instance) error
	Remove(ctx context.Context, inst instance) error
	Versions(ctx context.Context) map[string]string
}

// appConfig is the workload shared by every runtime.
type appConfig struct {
	BinaryPath   string
	BinarySHA256 string
	GuestPort    int
	Memory       string
}

func newRuntime(spec runtimeSpec, jerboaBin string) benchRuntime {
	if spec.Kind == "docker" {
		return &dockerRuntime{spec: spec}
	}
	if b := spec.Opts["bin"]; b != "" {
		jerboaBin = b
	}
	return &jerboaRuntime{spec: spec, bin: jerboaBin}
}

// command runs name with args and returns trimmed stdout. Stderr is folded
// into the error so failures are diagnosable from the report.
func command(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	// #nosec G204 -- this harness drives the CLIs and flags its operator passed in.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func splitArgs(s string) []string { return strings.Fields(s) }

// ---- Jerboa ----------------------------------------------------------------

type jerboaRuntime struct {
	spec      runtimeSpec
	bin       string
	ref       string
	guestPort int
}

func (j *jerboaRuntime) Spec() runtimeSpec { return j.spec }

func (j *jerboaRuntime) cli(args ...string) []string {
	if h := j.spec.Opts["host"]; h != "" {
		return append([]string{"--host", h}, args...)
	}
	return args
}

// buildLine matches the build summary: "sha256:<hex>  name:tag  ·  ...".
var buildLine = regexp.MustCompile(`(?m)^sha256:([0-9a-f]{64})\s+(\S+)`)

func (j *jerboaRuntime) Prepare(ctx context.Context, app appConfig) (imageInfo, error) {
	info := imageInfo{Layout: j.spec.Opts["layout"]}
	j.guestPort = app.GuestPort
	if img := j.spec.Opts["image"]; img != "" {
		j.ref = img
		info.Ref = img
		return info, nil
	}
	name := "jerboa-bench-" + sanitize(j.spec.Label)
	args := j.cli("build", app.BinaryPath, "--name", name, "--tag", app.BinarySHA256[:12],
		"--port", strconv.Itoa(app.GuestPort), "--memory", app.Memory)
	if l := j.spec.Opts["layout"]; l != "" {
		args = append(args, "--layout", l)
	}
	out, err := command(ctx, j.bin, args...)
	if err != nil {
		return info, err
	}
	m := buildLine.FindStringSubmatch(out)
	if m == nil {
		return info, fmt.Errorf("jerboa build: unexpected output %q", out)
	}
	j.ref = m[2]
	info.Ref = j.ref
	// The binary is packaged byte-for-byte from BinaryPath; re-hash it after
	// the build so a concurrent rebuild cannot swap it underneath us.
	sum, err := fileSHA256(app.BinaryPath)
	if err != nil {
		return info, err
	}
	if sum != app.BinarySHA256 {
		return info, fmt.Errorf("application binary changed during the jerboa build")
	}
	info.BinarySHA256 = sum
	store := j.spec.Opts["store"]
	if store == "" {
		home, _ := os.UserHomeDir()
		store = filepath.Join(home, ".jerboa", "images")
	}
	disk := filepath.Join(store, m[1], "disk.img")
	if err := measureFileSizes(ctx, disk, &info); err != nil {
		info.CompressionNote = "image sizes unavailable (remote daemon or custom store; pass store=...): " + err.Error()
	}
	return info, nil
}

func (j *jerboaRuntime) Start(ctx context.Context, name string, hostPort int) (instance, error) {
	args := j.cli("run", j.ref, "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, j.guestPort))
	if n := j.spec.Opts["network"]; n != "" {
		args = append(args, "--network", n)
	}
	args = append(args, splitArgs(j.spec.Opts["args"])...)
	out, err := command(ctx, j.bin, args...)
	if err != nil {
		return instance{}, err
	}
	id := strings.TrimSpace(lastLine(out))
	if id == "" {
		return instance{}, fmt.Errorf("jerboa run printed no VM id")
	}
	return instance{ID: id, HostPort: hostPort}, nil
}

func (j *jerboaRuntime) MemoryBytes(ctx context.Context, inst instance) (int64, string, error) {
	out, err := command(ctx, j.bin, j.cli("--output", "json", "stats", inst.ID)...)
	if err != nil {
		return 0, "", err
	}
	var st struct {
		MemBytes int64  `json:"mem_bytes"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return 0, "", fmt.Errorf("parse jerboa stats: %w", err)
	}
	return st.MemBytes, "vmm-rss:" + st.Source, nil
}

func (j *jerboaRuntime) GuestIP(ctx context.Context, inst instance) (string, error) {
	out, err := command(ctx, j.bin, j.cli("inspect", inst.ID)...)
	if err != nil {
		return "", err
	}
	var d struct {
		IPAddress string `json:"ip_address"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return "", fmt.Errorf("parse jerboa inspect: %w", err)
	}
	return d.IPAddress, nil
}

func (j *jerboaRuntime) Stop(ctx context.Context, inst instance) error {
	_, err := command(ctx, j.bin, j.cli("stop", inst.ID)...)
	return err
}

// WaitStopped polls until the VM leaves the stopping state. jerboa stop can
// return once the hypervisor process is signaled, before the daemon's monitor
// has recorded the VM as stopped, and a remove in that window is rejected with
// "vm is stopping, must be stopped first".
func (j *jerboaRuntime) WaitStopped(ctx context.Context, inst instance) error {
	for {
		out, err := command(ctx, j.bin, j.cli("inspect", inst.ID)...)
		if err != nil {
			return err
		}
		var d struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			return fmt.Errorf("parse jerboa inspect: %w", err)
		}
		if d.State == "stopped" || d.State == "created" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vm %s is still %s: %w", inst.ID, d.State, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (j *jerboaRuntime) Remove(ctx context.Context, inst instance) error {
	_, err := command(ctx, j.bin, j.cli("rm", inst.ID)...)
	return err
}

func (j *jerboaRuntime) Versions(ctx context.Context) map[string]string {
	v := map[string]string{}
	if out, err := command(ctx, j.bin, j.cli("version")...); err == nil {
		v["jerboa"] = out
	}
	return v
}

// ---- Docker ----------------------------------------------------------------

type dockerRuntime struct {
	spec      runtimeSpec
	ref       string
	guestPort int
}

func (d *dockerRuntime) Spec() runtimeSpec { return d.spec }

func (d *dockerRuntime) Prepare(ctx context.Context, app appConfig) (imageInfo, error) {
	d.guestPort = app.GuestPort
	if img := d.spec.Opts["image"]; img != "" {
		d.ref = img
		return imageInfo{Ref: img}, nil
	}
	dir, err := os.MkdirTemp("", "jerboa-bench-docker-")
	if err != nil {
		return imageInfo{}, fmt.Errorf("docker build context: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	data, err := os.ReadFile(app.BinaryPath)
	if err != nil {
		return imageInfo{}, fmt.Errorf("read the benchmark binary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app"), data, 0o755); err != nil {
		return imageInfo{}, fmt.Errorf("stage the benchmark binary: %w", err)
	}
	dockerfile := fmt.Sprintf("FROM scratch\nCOPY app /app\nEXPOSE %d\nENTRYPOINT [\"/app\"]\n", app.GuestPort)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return imageInfo{}, fmt.Errorf("write the Dockerfile: %w", err)
	}
	d.ref = "jerboa-bench-" + sanitize(d.spec.Label) + ":" + app.BinarySHA256[:12]
	platform := "linux/" + runtime.GOARCH
	if _, err := command(ctx, "docker", "build", "--platform", platform, "-t", d.ref, dir); err != nil {
		return imageInfo{}, err
	}
	info := imageInfo{Ref: d.ref}
	sum, err := d.binaryHashInImage(ctx)
	if err != nil {
		return info, err
	}
	if sum != app.BinarySHA256 {
		return info, fmt.Errorf("docker image %s holds a different binary (%s) than the benchmark app (%s)", d.ref, sum, app.BinarySHA256)
	}
	info.BinarySHA256 = sum
	if out, err := command(ctx, "docker", "image", "inspect", "-f", "{{.Size}}", d.ref); err == nil {
		info.LogicalBytes, _ = strconv.ParseInt(out, 10, 64)
	}
	if err := d.measureCompressed(ctx, &info); err != nil {
		info.CompressionNote = err.Error()
	}
	return info, nil
}

// binaryHashInImage copies /app out of a created (never started) container.
func (d *dockerRuntime) binaryHashInImage(ctx context.Context) (string, error) {
	id, err := command(ctx, "docker", "create", d.ref)
	if err != nil {
		return "", err
	}
	defer func() { _, _ = command(context.Background(), "docker", "rm", id) }()
	// #nosec G204 -- the image reference comes from this harness's own flags.
	cmd := exec.CommandContext(ctx, "docker", "cp", id+":/app", "-")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker cp: %w", err)
	}
	tr := tar.NewReader(bytes.NewReader(out))
	if _, err := tr.Next(); err != nil {
		return "", fmt.Errorf("read docker cp archive: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, tr); err != nil {
		return "", fmt.Errorf("hash the image's binary: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// measureCompressed saves the image and compresses it the same way the
// Jerboa disk is measured, so download-size numbers are comparable.
func (d *dockerRuntime) measureCompressed(ctx context.Context, info *imageInfo) error {
	f, err := os.CreateTemp("", "jerboa-bench-save-*.tar")
	if err != nil {
		return fmt.Errorf("docker save: %w", err)
	}
	path := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(path) }()
	if _, err := command(ctx, "docker", "save", "-o", path, d.ref); err != nil {
		return err
	}
	saved := imageInfo{}
	if err := measureFileSizes(ctx, path, &saved); err != nil {
		return err
	}
	info.GzipBytes, info.ZstdBytes, info.CompressionNote = saved.GzipBytes, saved.ZstdBytes, "compressed sizes are of `docker save` output"
	return nil
}

func (d *dockerRuntime) Start(ctx context.Context, name string, hostPort int) (instance, error) {
	args := []string{"run", "-d", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, d.guestPort)}
	args = append(args, splitArgs(d.spec.Opts["args"])...)
	args = append(args, d.ref)
	out, err := command(ctx, "docker", args...)
	if err != nil {
		return instance{}, err
	}
	return instance{ID: lastLine(out), HostPort: hostPort}, nil
}

func (d *dockerRuntime) MemoryBytes(ctx context.Context, inst instance) (int64, string, error) {
	out, err := command(ctx, "docker", "stats", "--no-stream", "--format", "{{.MemUsage}}", inst.ID)
	if err != nil {
		return 0, "", err
	}
	b, err := parseDockerMemUsage(out)
	return b, "cgroup:docker-stats", err
}

func (d *dockerRuntime) GuestIP(ctx context.Context, inst instance) (string, error) {
	return command(ctx, "docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", inst.ID)
}

func (d *dockerRuntime) Stop(ctx context.Context, inst instance) error {
	_, err := command(ctx, "docker", "stop", inst.ID)
	return err
}

// WaitStopped is a no-op: docker stop only returns once the container has exited.
func (d *dockerRuntime) WaitStopped(_ context.Context, _ instance) error { return nil }

func (d *dockerRuntime) Remove(ctx context.Context, inst instance) error {
	_, err := command(ctx, "docker", "rm", inst.ID)
	return err
}

func (d *dockerRuntime) Versions(ctx context.Context) map[string]string {
	v := map[string]string{}
	if out, err := command(ctx, "docker", "version", "-f", "{{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}} kernel={{.Server.KernelVersion}}"); err == nil {
		v["docker"] = out
	}
	return v
}

// parseDockerMemUsage parses "12.34MiB / 7.66GiB".
func parseDockerMemUsage(s string) (int64, error) {
	used, _, _ := strings.Cut(s, "/")
	used = strings.TrimSpace(used)
	units := []struct {
		suffix string
		mult   float64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(used, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(used, u.suffix), 64)
			if err != nil {
				return 0, fmt.Errorf("parse docker memory %q: %w", s, err)
			}
			return int64(v * u.mult), nil
		}
	}
	return 0, fmt.Errorf("parse docker memory %q: unknown unit", s)
}

// ---- helpers ---------------------------------------------------------------

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // user-provided benchmark binary
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, s)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// timed runs fn and returns its duration in milliseconds.
func timed(fn func() error) (float64, error) {
	t0 := time.Now()
	err := fn()
	return float64(time.Since(t0).Microseconds()) / 1000, err
}
