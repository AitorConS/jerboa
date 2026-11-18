//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The suite drives the real jerboa and jerboad binaries. Every daemon runs
// with a throwaway HOME, so nothing it stores or removes belongs to whoever
// runs the tests.
//
//	JERBOA_E2E_BIN         directory holding jerboa and jerboad
//	                       (default: dist, or dist/macos-arm64 on macOS)
//	JERBOA_E2E_TOOLS       kernel toolchain directory (default: staged from
//	                       kernel/output, or <bin>/tools on macOS)
//	JERBOA_E2E_HYPERVISOR  hypervisor of the tests that do not choose one
//	                       (default: firecracker where it can run, else qemu)
//	JERBOA_E2E_SLOW=1      also run the slow and host-level scenarios
//
// With CI set, a missing prerequisite fails the test instead of skipping it:
// on the runner a test must not pass by not running.

const (
	hvQEMU        = "qemu"
	hvFirecracker = "firecracker"
)

// suite is what every daemon of a test run shares.
var suite struct {
	repo  string // repository root
	bin   string // directory of jerboa and jerboad
	tools string // --tools-dir
	cache string // tool caches of sandboxed build steps (Linux)
	lacks []lack // prerequisites this machine does not meet
	seq   atomic.Uint32
}

func TestMain(m *testing.M) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(2)
	}
	suite.repo = repo
	suite.bin = os.Getenv("JERBOA_E2E_BIN")
	if suite.bin == "" {
		suite.bin = filepath.Join(repo, "dist")
		if runtime.GOOS == "darwin" {
			suite.bin = filepath.Join(repo, "dist", "macos-arm64")
		}
	}
	suite.tools = os.Getenv("JERBOA_E2E_TOOLS")
	cache, err := os.MkdirTemp("", "jerboa-e2e-cache-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(2)
	}
	suite.cache = cache
	suite.lacks = reportPreflight()
	code := m.Run()
	_ = os.RemoveAll(cache)
	os.Exit(code)
}

// missing skips the test for a prerequisite the machine lacks, and fails it
// on the CI runner, where every prerequisite is expected.
func missing(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("prerequisite missing on the CI runner: "+format, args...)
	}
	t.Skipf(format, args...)
}

// slow marks a scenario that only runs with JERBOA_E2E_SLOW=1 (nightly).
func slow(t *testing.T) {
	t.Helper()
	if os.Getenv("JERBOA_E2E_SLOW") == "" {
		t.Skip("slow scenario: set JERBOA_E2E_SLOW=1 to run it")
	}
}

func firecrackerKernel() string {
	return filepath.Join(suite.repo, "kernel", "output", "platform", "pc", "bin", "kernel.elf")
}

// firecrackerUsable reports whether this host can run the Linux Firecracker
// backend against the in-tree kernel, and why not otherwise.
func firecrackerUsable() (bool, string) {
	if runtime.GOOS != "linux" {
		return false, "the Firecracker backend under test is the Linux one"
	}
	if _, err := exec.LookPath("firecracker"); err != nil {
		return false, "firecracker not found in PATH"
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Sprintf("/dev/kvm unavailable: %v", err)
	}
	_ = f.Close()
	if _, err := os.Stat(firecrackerKernel()); err != nil {
		return false, fmt.Sprintf("in-tree Firecracker kernel missing (run make kernel): %v", err)
	}
	return true, ""
}

// defaultHypervisor is the hypervisor of tests that do not pick one.
func defaultHypervisor() string {
	if hv := os.Getenv("JERBOA_E2E_HYPERVISOR"); hv != "" {
		return hv
	}
	if ok, _ := firecrackerUsable(); ok {
		return hvFirecracker
	}
	return hvQEMU
}

// eachHypervisor runs fn once per hypervisor this platform has. On the CI
// runner Firecracker must be usable; elsewhere its run is skipped without it.
func eachHypervisor(t *testing.T, fn func(t *testing.T, hv string)) {
	t.Helper()
	t.Run(hvQEMU, func(t *testing.T) { fn(t, hvQEMU) })
	if runtime.GOOS != "linux" {
		return
	}
	t.Run(hvFirecracker, func(t *testing.T) {
		if ok, why := firecrackerUsable(); !ok {
			missing(t, "%s", why)
		}
		fn(t, hvFirecracker)
	})
}

// toolsDir returns the kernel toolchain the daemons use: the one this
// checkout built, so the suite tests this change's kernel.
func toolsDir(t *testing.T) string {
	t.Helper()
	if suite.tools != "" {
		return suite.tools
	}
	if runtime.GOOS == "darwin" {
		dir := filepath.Join(suite.bin, "tools")
		if _, err := os.Stat(filepath.Join(dir, "mkfs")); err != nil {
			missing(t, "kernel toolchain missing in %s (run scripts/build-macos.sh)", dir)
		}
		suite.tools = dir
		return dir
	}
	out := filepath.Join(suite.repo, "kernel", "output")
	srcs := map[string]string{
		"mkfs":       filepath.Join(out, "tools", "bin", "mkfs"),
		"kernel.img": filepath.Join(out, "platform", "pc", "bin", "kernel.img"),
		"boot.img":   filepath.Join(out, "platform", "pc", "boot", "boot.img"),
	}
	dir := filepath.Join(suite.cache, "tools")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for name, src := range srcs {
		data, err := os.ReadFile(src)
		if err != nil {
			missing(t, "kernel artifact missing (run make kernel && make -C kernel tools): %v", err)
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o755))
	}
	suite.tools = dir
	return dir
}

// env is one daemon with its own HOME, and the CLI pointed at it.
type env struct {
	t    *testing.T
	root string // short: Unix socket paths are limited to ~100 bytes
	home string
	sock string
	hv   string

	daemonEnv []string // extra environment of the daemon
	daemonArg []string // extra arguments of the daemon
	daemon    *exec.Cmd
	exited    chan struct{}
	logPath   string
	vmIDs     map[string]bool // every VM this env started, for the final sweep
}

type envOption func(*env)

func withHypervisor(hv string) envOption { return func(e *env) { e.hv = hv } }

func withDaemonEnv(kv ...string) envOption {
	return func(e *env) { e.daemonEnv = append(e.daemonEnv, kv...) }
}

func withDaemonArgs(args ...string) envOption {
	return func(e *env) { e.daemonArg = append(e.daemonArg, args...) }
}

// withHome puts the daemon's HOME at dir (a mount the test prepared).
func withHome(dir string) envOption { return func(e *env) { e.home = dir } }

// newEnv starts a daemon in a fresh HOME. When the test ends every VM is
// stopped, the daemon is stopped and the HOME is deleted.
func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	for _, name := range []string{"jerboa", "jerboad"} {
		if _, err := os.Stat(filepath.Join(suite.bin, name)); err != nil {
			missing(t, "%s not found in %s (run make build, or set JERBOA_E2E_BIN)", name, suite.bin)
		}
	}
	// Not t.TempDir(): its paths are too long for a Unix socket on macOS.
	root, err := os.MkdirTemp("/tmp", "jb-e2e-")
	require.NoError(t, err)
	e := &env{t: t, root: root, home: filepath.Join(root, "home"), sock: filepath.Join(root, "d.sock"), vmIDs: map[string]bool{}}
	for _, o := range opts {
		o(e)
	}
	if e.hv == "" {
		e.hv = defaultHypervisor()
	}
	if e.hv == hvFirecracker {
		if ok, why := firecrackerUsable(); !ok {
			missing(t, "%s", why)
		}
	}
	require.NoError(t, os.MkdirAll(e.home, 0o700))
	e.logPath = filepath.Join(root, "daemon.log")
	t.Cleanup(e.teardown)
	e.startDaemon()
	return e
}

// environ is the environment of the daemon and of every CLI call: the
// caller's without anything that points at another Jerboa.
func (e *env) environ(extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "JERBOA_") || strings.HasPrefix(name, "XDG_") || name == "HOME" {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"HOME="+e.home,
		"JERBOA_HOST=unix://"+e.sock,
		// One cache for the suite: sandboxed build steps keep their Go build
		// cache here instead of compiling the standard library per daemon.
		"XDG_CACHE_HOME="+suite.cache,
	)
	return append(out, extra...)
}

func (e *env) daemonArgs() []string {
	args := []string{"--host", "unix://" + e.sock, "--tools-dir", toolsDir(e.t), "--hypervisor", e.hv}
	if e.hv == hvFirecracker {
		args = append(args, "--fc-kernel", firecrackerKernel())
	}
	if runtime.GOOS == "darwin" {
		qemu, err := exec.LookPath("qemu-system-aarch64")
		if err != nil {
			missing(e.t, "qemu-system-aarch64 not found in PATH")
		}
		args = append(args, "--qemu", qemu)
	}
	return append(args, e.daemonArg...)
}

// startDaemon launches jerboad and waits until it answers.
func (e *env) startDaemon() {
	e.t.Helper()
	require.Nil(e.t, e.daemon, "daemon already running")
	logf, err := os.OpenFile(e.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(e.t, err)
	defer func() { _ = logf.Close() }()
	// A socket left by a daemon that was killed.
	_ = os.Remove(e.sock)
	cmd := exec.Command(filepath.Join(suite.bin, "jerboad"), e.daemonArgs()...)
	// Forwarding off unless a test asks: a guest must not depend on the
	// Internet, and an explicit empty upstream is how the daemon is told.
	cmd.Env = e.environ(append([]string{"JERBOA_DNS_UPSTREAM="}, e.daemonEnv...)...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	require.NoError(e.t, cmd.Start(), "start jerboad")
	e.daemon = cmd
	e.exited = make(chan struct{})
	go func(done chan struct{}) { _ = cmd.Wait(); close(done) }(e.exited)

	deadline := time.Now().Add(90 * time.Second)
	for {
		if res := e.run("status"); res.code == 0 {
			return
		}
		select {
		case <-e.exited:
			e.daemon = nil
			e.t.Fatalf("jerboad exited during startup; log:\n%s", e.daemonLog())
		default:
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("jerboad did not answer within 90s; log:\n%s", e.daemonLog())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// tryStartDaemon launches jerboad and reports whether it came up, for tests
// of a daemon that may refuse to start.
func (e *env) tryStartDaemon(wait time.Duration) bool {
	e.t.Helper()
	require.Nil(e.t, e.daemon, "daemon already running")
	logf, err := os.OpenFile(e.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(e.t, err)
	defer func() { _ = logf.Close() }()
	_ = os.Remove(e.sock)
	cmd := exec.Command(filepath.Join(suite.bin, "jerboad"), e.daemonArgs()...)
	cmd.Env = e.environ(append([]string{"JERBOA_DNS_UPSTREAM="}, e.daemonEnv...)...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	require.NoError(e.t, cmd.Start(), "start jerboad")
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if res := e.run("status"); res.code == 0 {
			e.daemon, e.exited = cmd, exited
			return true
		}
		select {
		case <-exited:
			return false
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	<-exited
	return false
}

// killDaemon ends the daemon the way a crash or the OOM killer does.
func (e *env) killDaemon() {
	e.t.Helper()
	require.NotNil(e.t, e.daemon, "no daemon to kill")
	require.NoError(e.t, e.daemon.Process.Kill())
	<-e.exited
	e.daemon = nil
}

// stopDaemon ends the daemon the way a service manager does.
func (e *env) stopDaemon() {
	e.t.Helper()
	if e.daemon == nil {
		return
	}
	_ = e.daemon.Process.Signal(syscall.SIGTERM)
	select {
	case <-e.exited:
	case <-time.After(30 * time.Second):
		_ = e.daemon.Process.Kill()
		<-e.exited
		e.daemon = nil
		e.t.Errorf("jerboad did not exit within 30s of SIGTERM; log:\n%s", e.daemonLog())
		return
	}
	e.daemon = nil
}

func (e *env) restartDaemon() {
	e.t.Helper()
	e.stopDaemon()
	e.startDaemon()
}

func (e *env) daemonLog() string {
	data, err := os.ReadFile(e.logPath)
	if err != nil {
		return "(no daemon log: " + err.Error() + ")"
	}
	const keep = 48 << 10
	if len(data) > keep {
		data = append([]byte("[...]\n"), data[len(data)-keep:]...)
	}
	return string(data)
}

// tail returns the last max bytes of s.
func tail(s string, max int) string {
	if len(s) > max {
		return "[...]" + s[len(s)-max:]
	}
	return s
}

// diagnostics describes what a failed test left: the VMs as the daemon
// lists them, the VMM processes that exist, each VM's console and, for
// Firecracker, the VMM's own log. Enough to tell from a CI log alone
// whether the daemon, the VMM or the guest is at fault.
func (e *env) diagnostics() string {
	var b strings.Builder
	if e.daemon == nil {
		b.WriteString("(the daemon is not running)\n")
	} else {
		res := e.runFor(20*time.Second, "ps")
		fmt.Fprintf(&b, "jerboa ps (exit %d):\n%s%s\n", res.code, res.stdout, res.stderr)
		res = e.runFor(20*time.Second, "network", "ls")
		fmt.Fprintf(&b, "jerboa network ls (exit %d):\n%s%s\n", res.code, res.stdout, res.stderr)
	}
	ids := map[string]bool{}
	for id := range e.vmIDs {
		ids[id] = true
	}
	for _, v := range e.tryPS() {
		ids[v.ID] = true
	}
	for id := range ids {
		fmt.Fprintf(&b, "VM %s: VMM processes %v\n", id, vmmPIDs(id))
		if e.daemon != nil {
			res := e.runFor(20*time.Second, "inspect", id)
			fmt.Fprintf(&b, "  inspect (exit %d): %s\n", res.code, strings.Join(strings.Fields(res.all()), " "))
			res = e.runFor(20*time.Second, "logs", id)
			fmt.Fprintf(&b, "  console (exit %d):\n%s\n", res.code, tail(res.all(), 4<<10))
		}
		// Linux Firecracker keeps the VMM's log beside its sockets.
		logs, _ := filepath.Glob(filepath.Join(os.TempDir(), "jerboa-run-*", "fc-"+id+"-vmm.log"))
		for _, path := range logs {
			if data, err := os.ReadFile(path); err == nil {
				fmt.Fprintf(&b, "  %s:\n%s\n", path, tail(string(data), 4<<10))
			}
		}
	}
	if runtime.GOOS == "linux" {
		for _, args := range [][]string{{"ip", "-br", "link"}, {"nft", "list", "table", "ip", "jerboa"}} {
			out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
			fmt.Fprintf(&b, "%s (%v):\n%s\n", strings.Join(args, " "), err, tail(string(out), 4<<10))
		}
	}
	return b.String()
}

// teardown leaves nothing of the env behind: VMs, VMM processes, networks
// (bridges and firewall entries on Linux), the daemon and its HOME.
func (e *env) teardown() {
	if e.t.Failed() {
		e.t.Logf("daemon log of %s:\n%s", e.root, e.daemonLog())
		e.t.Logf("state of %s when the test failed:\n%s", e.root, e.diagnostics())
	}
	if e.daemon == nil {
		// The daemon is what removes bridges and taps: bring one up to clean.
		_ = e.tryStartDaemon(30 * time.Second)
	}
	if e.daemon != nil {
		for _, v := range e.tryPS() {
			e.vmIDs[v.ID] = true
			_ = e.run("stop", "--force", v.ID)
		}
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			running := false
			for _, v := range e.tryPS() {
				running = running || v.State != "stopped"
			}
			if !running {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		for _, v := range e.tryPS() {
			_ = e.run("rm", v.ID)
		}
		var nets []struct {
			Name string `json:"name"`
		}
		if res := e.run("--output", "json", "network", "ls"); res.code == 0 {
			_ = json.Unmarshal([]byte(res.stdout), &nets)
		}
		for _, n := range nets {
			_ = e.run("network", "rm", n.Name)
		}
		e.stopDaemon()
	}
	for id := range e.vmIDs {
		for _, pid := range vmmPIDs(id) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	_ = os.RemoveAll(e.root)
}

type result struct {
	stdout, stderr string
	code           int
}

func (r result) all() string { return r.stdout + r.stderr }

// command returns the CLI invocation for args, not yet started.
func (e *env) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, filepath.Join(suite.bin, "jerboa"), args...)
	cmd.Env = e.environ()
	cmd.Dir = e.root
	return cmd
}

// run executes the CLI and returns what it printed and its exit status.
func (e *env) run(args ...string) result {
	return e.runFor(3*time.Minute, args...)
}

func (e *env) runFor(timeout time.Duration, args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := e.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		res.code = -1
		res.stderr += fmt.Sprintf("\n[e2e: jerboa %s timed out after %s]", strings.Join(args, " "), timeout)
	case errors.As(err, &exitErr):
		res.code = exitErr.ExitCode()
	default:
		res.code = -1
		res.stderr += "\n[e2e: " + err.Error() + "]"
	}
	return res
}

// ok runs the CLI, requires success and returns its standard output.
func (e *env) ok(args ...string) string {
	e.t.Helper()
	res := e.run(args...)
	require.Zerof(e.t, res.code, "jerboa %s failed (exit %d)\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), res.code, res.stdout, res.stderr)
	return res.stdout
}

// fails runs the CLI, requires a failure exit that is not a timeout or a
// crash, and returns everything it printed.
func (e *env) fails(args ...string) string {
	e.t.Helper()
	res := e.run(args...)
	require.Equalf(e.t, 1, res.code, "jerboa %s: want exit 1, got %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), res.code, res.stdout, res.stderr)
	return res.all()
}

// jsonOut runs the CLI with --output json and decodes what it prints.
func (e *env) jsonOut(v any, args ...string) {
	e.t.Helper()
	out := e.ok(append([]string{"--output", "json"}, args...)...)
	require.NoErrorf(e.t, json.Unmarshal([]byte(out), v), "jerboa %s printed no valid JSON:\n%s", strings.Join(args, " "), out)
}

type vmRow struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Image string `json:"image"`
}

func (e *env) ps() []vmRow {
	e.t.Helper()
	var rows []vmRow
	e.jsonOut(&rows, "ps")
	return rows
}

// tryPS is ps for cleanup paths, which must not fail the test again.
func (e *env) tryPS() []vmRow {
	var rows []vmRow
	if res := e.run("--output", "json", "ps"); res.code == 0 {
		_ = json.Unmarshal([]byte(res.stdout), &rows)
	}
	return rows
}

// vmState returns the state of the VM named or identified by ref, or "" when
// the daemon no longer knows it.
func (e *env) vmState(ref string) string {
	e.t.Helper()
	for _, v := range e.ps() {
		if v.ID == ref || v.Name == ref {
			return v.State
		}
	}
	return ""
}

func (e *env) waitState(ref, want string, within time.Duration) {
	e.t.Helper()
	var got string
	require.Eventuallyf(e.t, func() bool {
		got = e.vmState(ref)
		return got == want
	}, within, 200*time.Millisecond, "VM %s did not reach state %q", ref, want)
}

// waitGone waits until the daemon no longer lists the VM.
func (e *env) waitGone(ref string, within time.Duration) {
	e.t.Helper()
	require.Eventuallyf(e.t, func() bool { return e.vmState(ref) == "" },
		within, 200*time.Millisecond, "VM %s is still listed", ref)
}

func (e *env) logs(ref string) string {
	e.t.Helper()
	return e.ok("logs", ref)
}

// waitLog waits until the VM's console holds want and returns the console.
func (e *env) waitLog(ref, want string, within time.Duration) string {
	e.t.Helper()
	var logs string
	deadline := time.Now().Add(within)
	for {
		res := e.run("logs", ref)
		logs = res.stdout
		if res.code == 0 && strings.Contains(logs, want) {
			return logs
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("console of %s never showed %q within %s; console:\n%s\n%s", ref, want, within, logs, res.stderr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// stopAndRemove stops a VM gracefully and removes it.
func (e *env) stopAndRemove(ref string) {
	e.t.Helper()
	e.ok("stop", ref)
	e.waitState(ref, "stopped", 60*time.Second)
	e.ok("rm", ref)
}

// buildArgs returns the arguments that build dir as image name for the
// guests this platform runs.
func buildArgs(dir, name string) []string {
	args := []string{"build", dir, "--name", name}
	if runtime.GOOS == "darwin" {
		args = append(args, "--platform", "linux/arm64")
	}
	return args
}

// writeProject writes a Go project that builds into a unikernel image: the
// probe guest unless main is given.
func writeProject(t *testing.T, dir, mainSrc, extraBuild string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	if mainSrc == "" {
		data, err := os.ReadFile(filepath.Join("testdata", "probe", "main.go"))
		require.NoError(t, err)
		mainSrc = string(data)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSrc), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.25\n"), 0o600))
	toml := "[build]\nlang = \"go\"\ndirs = [\"/data\"]\n" + extraBuild
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unikernel.toml"), []byte(toml), 0o600))
}

// buildProbe builds the probe guest as image "probe" and returns its digest.
func (e *env) buildProbe() string {
	e.t.Helper()
	dir := filepath.Join(e.root, "probe")
	writeProject(e.t, dir, "", "")
	e.ok(buildArgs(dir, "probe")...)
	return e.imageDigest("probe")
}

type imageRow struct {
	Name       string `json:"name"`
	Tag        string `json:"tag"`
	DiskDigest string `json:"disk_digest"`
	DiskSize   int64  `json:"disk_size"`
}

func (e *env) images() []imageRow {
	e.t.Helper()
	var rows []imageRow
	e.jsonOut(&rows, "images")
	return rows
}

func (e *env) imageDigest(name string) string {
	e.t.Helper()
	for _, img := range e.images() {
		if img.Name == name {
			return img.DiskDigest
		}
	}
	e.t.Fatalf("image %q is not in the store", name)
	return ""
}

// network creates a network on a subnet of its own and returns its name.
func (e *env) network() (name, subnet string) {
	e.t.Helper()
	requireGuestNetworking(e.t)
	n := suite.seq.Add(1)
	// Short: on Linux the bridge name derives from it, and interface names
	// hold 15 bytes.
	name = fmt.Sprintf("e%d", n)
	// 10.209.x.0/24 and up: away from the daemon's automatic ranges and from
	// what the runner's other jobs use.
	subnet = fmt.Sprintf("10.%d.%d.0/24", 209+int(n/250), n%250)
	e.ok("network", "create", name, "--subnet", subnet)
	return name, subnet
}

// guest is a running probe VM published on a loopback port of the host.
type guest struct {
	e    *env
	id   string
	name string
	port int
}

type guestOpts struct {
	name    string
	image   string // "probe" when empty
	network string
	volume  string // mounted at /data
	env     []string
	args    []string // further `jerboa run` arguments
	noWait  bool
}

// freePort returns a TCP port nothing listens on at loopback.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// startGuest runs the probe image detached, published on a free loopback
// port, and waits until it serves.
func (e *env) startGuest(o guestOpts) *guest {
	e.t.Helper()
	require.NotEmpty(e.t, o.network, "a published port needs a network")
	if o.image == "" {
		o.image = "probe"
	}
	g := &guest{e: e, name: o.name, port: freePort(e.t)}
	args := []string{"run", o.image, "--network", o.network, "-p", fmt.Sprintf("127.0.0.1:%d:8080", g.port)}
	if o.name != "" {
		args = append(args, "--name", o.name)
	}
	if o.volume != "" {
		args = append(args, "-v", o.volume+":/data")
	}
	for _, kv := range o.env {
		args = append(args, "-e", kv)
	}
	args = append(args, o.args...)
	g.id = strings.TrimSpace(e.ok(args...))
	require.NotEmpty(e.t, g.id, "jerboa run printed no VM id")
	e.vmIDs[g.id] = true
	if !o.noWait {
		g.waitReady()
	}
	return g
}

func (g *guest) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", g.port, path)
}

var httpClient = &http.Client{
	Timeout: 20 * time.Second,
	// One connection per request: a kept-alive connection to a VM that was
	// killed would fail the next request for a reason that is not the guest's.
	Transport: &http.Transport{DisableKeepAlives: true},
}

// httpGet returns the status and body of a GET, or an error.
func httpGet(rawURL string) (int, string, error) {
	resp, err := httpClient.Get(rawURL)
	if err != nil {
		return 0, "", fmt.Errorf("get %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, string(body), fmt.Errorf("read %s: %w", rawURL, err)
	}
	return resp.StatusCode, string(body), nil
}

// waitReady waits until the guest answers on its published port.
func (g *guest) waitReady() {
	g.e.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for {
		code, _, err := httpGet(g.url("/"))
		if err == nil && code == http.StatusOK {
			return
		}
		lastErr = err
		if time.Now().After(deadline) {
			res := g.e.run("logs", g.id)
			g.e.t.Fatalf("guest %s never answered on its published port %d (last error: %v, state %q); console:\n%s",
				g.id, g.port, lastErr, g.e.vmState(g.id), res.all())
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// get requires a 200 answer from the guest and returns the body.
func (g *guest) get(path string) string {
	g.e.t.Helper()
	code, body, err := httpGet(g.url(path))
	require.NoErrorf(g.e.t, err, "guest %s", g.id)
	require.Equalf(g.e.t, http.StatusOK, code, "guest %s answered %s with %d: %s", g.id, path, code, body)
	return body
}

// getJSON decodes the guest's answer to path into v.
func (g *guest) getJSON(path string, v any) {
	g.e.t.Helper()
	body := g.get(path)
	require.NoErrorf(g.e.t, json.Unmarshal([]byte(body), v), "guest %s answered %s with: %s", g.id, path, body)
}

type probeStatus struct {
	Marker  string `json:"marker"`
	Boots   int    `json:"boots"`
	Journal int    `json:"journal"`
	Torn    int64  `json:"torn"`
}

func (g *guest) status() probeStatus {
	g.e.t.Helper()
	var st probeStatus
	g.getJSON("/", &st)
	return st
}

func (g *guest) write(name, data string) {
	g.e.t.Helper()
	g.get("/write?name=" + url.QueryEscape(name) + "&data=" + url.QueryEscape(data))
}

func (g *guest) read(name string) string {
	g.e.t.Helper()
	return g.get("/read?name=" + url.QueryEscape(name))
}

type dialResult struct {
	OK     bool   `json:"ok"`
	Banner string `json:"banner"`
	Error  string `json:"error"`
}

// dial asks the guest to open a TCP connection to addr.
func (g *guest) dial(addr string) dialResult {
	g.e.t.Helper()
	var res dialResult
	g.getJSON("/dial?addr="+url.QueryEscape(addr), &res)
	return res
}

type resolveResult struct {
	Addrs []string `json:"addrs"`
	Error string   `json:"error"`
	MS    int64    `json:"ms"`
}

func (g *guest) resolve(name string) resolveResult {
	g.e.t.Helper()
	var res resolveResult
	g.getJSON("/resolve?name="+url.QueryEscape(name), &res)
	return res
}

type vmInspect struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	IPAddress string `json:"ip_address"`
	GatewayIP string `json:"gateway_ip"`
	ExitCode  *int   `json:"exit_code"`
}

func (e *env) inspect(ref string) vmInspect {
	e.t.Helper()
	var v vmInspect
	out := e.ok("inspect", ref)
	require.NoErrorf(e.t, json.Unmarshal([]byte(out), &v), "jerboa inspect %s printed:\n%s", ref, out)
	return v
}

// vmmPIDs returns the QEMU and Firecracker processes that run VM id: the
// daemon names each VM's control socket after the VM, on the VMM's command
// line.
func vmmPIDs(id string) []int {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.Contains(line, "-"+id) {
			continue
		}
		// The daemon may launch the VMM through itself (jerboad
		// __jerboa-vmm-exec ...) before it execs; either way one of the
		// first arguments names the VMM.
		head := strings.Join(fields[1:min(len(fields), 4)], " ")
		if !strings.Contains(head, "qemu") && !strings.Contains(head, "firecracker") {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids
}

// vmmPID returns the one VMM process of a running VM.
func (e *env) vmmPID(id string) int {
	e.t.Helper()
	pids := vmmPIDs(id)
	require.Lenf(e.t, pids, 1, "want exactly one VMM process for VM %s", id)
	return pids[0]
}

// waitNoVMM waits until no VMM process of the VM is left.
func (e *env) waitNoVMM(id string, within time.Duration) {
	e.t.Helper()
	require.Eventuallyf(e.t, func() bool { return len(vmmPIDs(id)) == 0 },
		within, 200*time.Millisecond, "a VMM process of VM %s is still running", id)
}

type finding struct {
	Check    string `json:"check"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Hint     string `json:"hint"`
}

type doctorReport struct {
	Findings []finding `json:"findings"`
	OK       bool      `json:"ok"`
}

// doctor runs `jerboa doctor` and returns its report and exit status.
func (e *env) doctor(args ...string) (doctorReport, int) {
	e.t.Helper()
	res := e.run(append([]string{"--output", "json", "doctor"}, args...)...)
	var rep doctorReport
	require.NoErrorf(e.t, json.Unmarshal([]byte(res.stdout), &rep), "jerboa doctor (exit %d) printed no JSON report:\n%s\n%s", res.code, res.stdout, res.stderr)
	return rep, res.code
}

// stateChecks are the doctor's checks of what the daemon stores and runs;
// the others describe the machine (free disk space, virtualization).
var stateChecks = map[string]bool{
	"daemon": true, "daemon state": true, "images": true, "vms": true,
	"volumes": true, "networks": true, "snapshots": true,
}

// problems returns the warnings and errors about the daemon and its state.
func (r doctorReport) problems() []finding {
	var out []finding
	for _, f := range r.Findings {
		if stateChecks[f.Check] && (f.Severity == "warning" || f.Severity == "error") {
			out = append(out, f)
		}
	}
	return out
}

// errorsOf returns the error findings of one check.
func (r doctorReport) errorsOf(check string) []finding {
	var out []finding
	for _, f := range r.Findings {
		if f.Check == check && f.Severity == "error" {
			out = append(out, f)
		}
	}
	return out
}

// requireHealthy fails unless the doctor finds the daemon and everything it
// stores in order.
func (e *env) requireHealthy() {
	e.t.Helper()
	rep, _ := e.doctor()
	require.Emptyf(e.t, rep.problems(), "jerboa doctor found problems in the daemon's state")
}

// jsonUnmarshal decodes s into v.
func jsonUnmarshal(s string, v any) error {
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("decode %q: %w", s, err)
	}
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }
