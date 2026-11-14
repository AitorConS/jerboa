package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderDF(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, renderDF(&out, api.SystemDF{
		DataDir: "/home/ana/.jerboa", Total: 500 << 30, Free: 120 << 30,
		Usage: []api.SystemUsage{
			{Kind: "images", Count: 5, Active: 2, Bytes: 4 << 30, Reclaimable: 1 << 30},
			{Kind: "volumes", Count: 1, Active: 1, Bytes: 2 << 20},
			{Kind: "future-kind", Count: 3, Bytes: 10},
		},
	}))
	got := out.String()
	lines := strings.Split(strings.TrimSpace(got), "\n")
	assert.Equal(t, []string{"TYPE", "TOTAL", "ACTIVE", "SIZE", "RECLAIMABLE"}, strings.Fields(lines[0]))
	assert.Equal(t, []string{"Images", "5", "2", "4.0GB", "1.0GB", "(25%)"}, strings.Fields(lines[1]))
	assert.Equal(t, []string{"Volumes", "1", "1", "2.0MB", "0B"}, strings.Fields(lines[2]))
	assert.Equal(t, "future-kind", strings.Fields(lines[3])[0], "a kind this CLI does not know is still shown")
	assert.Contains(t, got, "Total: 4.0GB, of which 1.0GB can be reclaimed with `jerboa system prune`.")
	assert.Contains(t, got, "Disk:  120.0GB free of 500.0GB (/home/ana/.jerboa)")
}

func TestRenderDF_NoFilesystemInfo(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, renderDF(&out, api.SystemDF{}))
	assert.NotContains(t, out.String(), "Disk:")
	assert.Contains(t, out.String(), "Total: 0B")
}

func TestReclaimableCell(t *testing.T) {
	assert.Equal(t, "0B", reclaimableCell(0, 100))
	assert.Equal(t, "0B", reclaimableCell(0, 0))
	assert.Equal(t, "50B (50%)", reclaimableCell(50, 100))
	assert.Equal(t, "100B (100%)", reclaimableCell(100, 100))
	assert.Equal(t, "5B", reclaimableCell(5, 0), "no percentage of nothing")
}

func TestRenderPrune(t *testing.T) {
	res := api.SystemPruneResult{
		Reclaimed: 3 << 20,
		Removed: []api.SystemPruneItem{
			{Kind: "boot-disk", Name: "fc-abc-rootfs.img", Bytes: 1 << 20},
			{Kind: "build-cache", Name: "aaaaaaaaaaaa", Bytes: 512 << 10},
			{Kind: "build-cache", Name: "bbbbbbbbbbbb", Bytes: 512 << 10},
			{Kind: "image", Name: "web:latest", Bytes: 1 << 20},
			{Kind: "mystery", Name: "thing", Bytes: 0},
		},
		Errors: []string{"volume data: device busy"},
	}
	var out bytes.Buffer
	renderPrune(&out, res)
	got := out.String()
	assert.True(t, strings.HasPrefix(got, "Removed:\n"))
	assert.Contains(t, got, "Boot disk")
	assert.Contains(t, got, "web:latest")
	assert.Contains(t, got, "2 unused blobs", "cache blobs are summed into one line")
	assert.Contains(t, got, "1.0MB")
	assert.NotContains(t, got, "aaaaaaaaaaaa")
	assert.Contains(t, got, "mystery")
	assert.Contains(t, got, "could not remove volume data: device busy")
	assert.True(t, strings.HasSuffix(got, "Reclaimed: 3.0MB\n"))

	out.Reset()
	res.DryRun = true
	renderPrune(&out, res)
	assert.True(t, strings.HasPrefix(out.String(), "Would remove:\n"))
	assert.True(t, strings.HasSuffix(out.String(), "Would reclaim: 3.0MB\n"))
}

func TestRenderPrune_Nothing(t *testing.T) {
	var out bytes.Buffer
	renderPrune(&out, api.SystemPruneResult{DryRun: true})
	assert.Equal(t, "Nothing to prune.\n", out.String())
}

func TestRenderPrune_OnlyErrors(t *testing.T) {
	var out bytes.Buffer
	renderPrune(&out, api.SystemPruneResult{Errors: []string{"vm app: busy"}})
	assert.NotContains(t, out.String(), "Removed:")
	assert.Contains(t, out.String(), "could not remove vm app: busy")
}

func TestErrOldDaemon(t *testing.T) {
	err := errOldDaemon("system df", &api.RPCError{Code: -32601, Message: "method not found: System.DF"})
	assert.EqualError(t, err, "system df: the running daemon is older than this CLI and does not support it; restart the daemon")

	other := &api.RPCError{Code: -32000, Message: "boom"}
	err = errOldDaemon("system df", other)
	assert.ErrorIs(t, err, other)
	assert.Contains(t, err.Error(), "boom")
}

func TestSystemAndDoctorAreRegistered(t *testing.T) {
	root := newRootCmd()
	find := func(args ...string) *cobra.Command {
		cmd, _, err := root.Find(args)
		require.NoError(t, err)
		return cmd
	}
	assert.Equal(t, "df", find("system", "df").Name())
	prune := find("system", "prune")
	assert.Equal(t, "prune", prune.Name())
	for _, flag := range []string{"vms", "all", "volumes", "dry-run", "yes"} {
		assert.NotNil(t, prune.Flags().Lookup(flag), flag)
	}
	assert.Equal(t, "a", prune.Flags().Lookup("all").Shorthand)
	doctor := find("doctor")
	assert.NotNil(t, doctor.Flags().Lookup("deep"))

	assert.True(t, needsDaemon(find("system", "df")), "system commands go through the daemon")
	assert.False(t, needsDaemon(doctor), "the doctor reports an absent daemon itself")
}

// --- doctor ------------------------------------------------------------------

func healthyEnv() doctorEnv {
	return doctorEnv{
		goos: "linux", goarch: "amd64", cliVersion: "v0.60.0", configPath: "/home/ana/.config/jerboa/config.toml",
		loadConfig: func(string) error { return nil },
		credentials: func() (string, os.FileMode, bool, error) {
			return "/home/ana/.jerboa/credentials.json", 0o600, true, nil
		},
		kvmPresent: func() bool { return true },
	}
}

func severities(findings []api.SystemFinding) map[string]string {
	out := map[string]string{}
	for _, f := range findings {
		out[f.Check] = f.Severity
	}
	return out
}

func find(t *testing.T, findings []api.SystemFinding, check string) api.SystemFinding {
	t.Helper()
	for _, f := range findings {
		if f.Check == check {
			return f
		}
	}
	t.Fatalf("no finding for %q", check)
	return api.SystemFinding{}
}

func TestClientFindings_Healthy(t *testing.T) {
	got := clientFindings(healthyEnv())
	assert.Equal(t, map[string]string{"cli": "ok", "config": "ok", "credentials": "ok", "virtualization": "ok"}, severities(got))
	assert.Contains(t, find(t, got, "cli").Message, "v0.60.0 on linux/amd64")
	for _, f := range got {
		assert.Empty(t, f.Hint, f.Check)
	}
}

func TestClientFindings_DamagedConfig(t *testing.T) {
	env := healthyEnv()
	env.loadConfig = func(string) error { return errors.New("config: parse /x/config.toml: expected '='") }
	f := find(t, clientFindings(env), "config")
	assert.Equal(t, api.SeverityError, f.Severity)
	assert.Contains(t, f.Message, "/x/config.toml")
	assert.NotEmpty(t, f.Hint)
}

func TestClientFindings_Credentials(t *testing.T) {
	env := healthyEnv()
	env.credentials = func() (string, os.FileMode, bool, error) { return "/c.json", 0, false, nil }
	f := find(t, clientFindings(env), "credentials")
	assert.Equal(t, api.SeverityOK, f.Severity)
	assert.Contains(t, f.Message, "not logged in")

	env.credentials = func() (string, os.FileMode, bool, error) { return "/c.json", 0o644, true, nil }
	f = find(t, clientFindings(env), "credentials")
	assert.Equal(t, api.SeverityWarning, f.Severity)
	assert.Contains(t, f.Message, "0644")
	assert.Equal(t, "chmod 600 /c.json", f.Hint)

	// Windows has no POSIX modes to complain about.
	env.goos = "windows"
	assert.Equal(t, api.SeverityOK, find(t, clientFindings(env), "credentials").Severity)

	env.credentials = func() (string, os.FileMode, bool, error) {
		return "/c.json", 0o600, true, errors.New("credentials: /c.json: unexpected end of JSON input")
	}
	f = find(t, clientFindings(env), "credentials")
	assert.Equal(t, api.SeverityWarning, f.Severity)
	assert.Contains(t, f.Hint, "jerboa login")
}

func TestClientFindings_Virtualization(t *testing.T) {
	env := healthyEnv()
	env.kvmPresent = func() bool { return false }
	f := find(t, clientFindings(env), "virtualization")
	assert.Equal(t, api.SeverityWarning, f.Severity)
	assert.Contains(t, f.Message, "/dev/kvm is missing")

	env.goos, env.goarch = "darwin", "arm64"
	assert.Equal(t, api.SeverityOK, find(t, clientFindings(env), "virtualization").Severity)
	env.goarch = "amd64"
	assert.Equal(t, api.SeverityWarning, find(t, clientFindings(env), "virtualization").Severity)

	env.goos = "windows"
	_, has := severities(clientFindings(env))["virtualization"]
	assert.False(t, has, "on Windows the daemon's own bring-up reports what is missing")
}

type fakeDaemon struct {
	info     api.DaemonInfo
	infoErr  error
	check    api.SystemCheckResult
	checkErr error
	deep     bool
}

func (f *fakeDaemon) DaemonInfo(context.Context) (api.DaemonInfo, error) { return f.info, f.infoErr }
func (f *fakeDaemon) SystemCheck(_ context.Context, p api.SystemCheckParams) (api.SystemCheckResult, error) {
	f.deep = p.Deep
	return f.check, f.checkErr
}

func TestDaemonFindings_Unreachable(t *testing.T) {
	for goos, want := range map[string]string{"linux": "systemctl", "darwin": "jerboa daemon start", "windows": "WSL2"} {
		env := healthyEnv()
		env.goos = goos
		got := daemonFindings(context.Background(), env, nil, errors.New("dial unix /run/jerboad.sock: connect: no such file or directory"), false)
		require.Len(t, got, 1)
		assert.Equal(t, "daemon", got[0].Check)
		assert.Equal(t, api.SeverityError, got[0].Severity)
		assert.Contains(t, got[0].Message, "not reachable")
		assert.Contains(t, got[0].Hint, want, goos)
	}
}

func TestDaemonFindings_Healthy(t *testing.T) {
	d := &fakeDaemon{info: api.DaemonInfo{Version: "0.60.0"}, check: api.SystemCheckResult{Findings: []api.SystemFinding{
		{Check: "images", Severity: api.SeverityOK, Message: "3 images readable"},
	}}}
	got := daemonFindings(context.Background(), healthyEnv(), d, nil, true)
	assert.Equal(t, map[string]string{"daemon": "ok", "images": "ok"}, severities(got), "v0.60.0 and 0.60.0 are the same version")
	assert.True(t, d.deep, "--deep reaches the daemon")
}

func TestDaemonFindings_VersionMismatch(t *testing.T) {
	d := &fakeDaemon{info: api.DaemonInfo{Version: "v0.59.0"}}
	f := find(t, daemonFindings(context.Background(), healthyEnv(), d, nil, false), "daemon")
	assert.Equal(t, api.SeverityWarning, f.Severity)
	assert.Contains(t, f.Message, "v0.59.0")
	assert.Contains(t, f.Message, "v0.60.0")
	assert.Contains(t, f.Hint, "restart the daemon")

	// Development builds on either side are not a mismatch worth reporting.
	for _, pair := range [][2]string{{"dev", "v0.59.0"}, {"v0.60.0", "dev"}, {"v0.60.0", ""}} {
		env := healthyEnv()
		env.cliVersion = pair[0]
		d := &fakeDaemon{info: api.DaemonInfo{Version: pair[1]}}
		assert.Equal(t, api.SeverityOK, find(t, daemonFindings(context.Background(), env, d, nil, false), "daemon").Severity, "%v", pair)
	}
}

func TestDaemonFindings_DaemonDoesNotAnswer(t *testing.T) {
	d := &fakeDaemon{infoErr: errors.New("decode response: EOF")}
	got := daemonFindings(context.Background(), healthyEnv(), d, nil, false)
	require.Len(t, got, 1, "nothing else is asked of a daemon that does not answer")
	assert.Equal(t, api.SeverityError, got[0].Severity)
}

func TestDaemonFindings_OlderDaemon(t *testing.T) {
	d := &fakeDaemon{info: api.DaemonInfo{Version: "v0.60.0"}, checkErr: &api.RPCError{Code: -32601, Message: "method not found: System.Check"}}
	f := find(t, daemonFindings(context.Background(), healthyEnv(), d, nil, false), "daemon state")
	assert.Equal(t, api.SeverityInfo, f.Severity, "an older daemon is not a problem in itself")
	assert.Contains(t, f.Hint, "restart the daemon")

	d.checkErr = errors.New("decode response: EOF")
	f = find(t, daemonFindings(context.Background(), healthyEnv(), d, nil, false), "daemon state")
	assert.Equal(t, api.SeverityError, f.Severity)
}

func TestRenderDoctor(t *testing.T) {
	var out bytes.Buffer
	renderDoctor(&out, []api.SystemFinding{
		{Check: "cli", Severity: api.SeverityOK, Message: "jerboa v0.60.0"},
		{Check: "disk space", Severity: api.SeverityWarning, Message: "only 3.0GB free", Hint: "jerboa system prune"},
		{Check: "images", Severity: api.SeverityError, Message: "image web:latest is corrupt", Hint: "jerboa rmi web:latest"},
		{Check: "kernel", Severity: api.SeverityInfo, Message: "not downloaded yet"},
		{Check: "x", Severity: "novel", Message: "from a newer daemon"},
	})
	lines := strings.Split(out.String(), "\n")
	assert.Equal(t, "[ ok ] cli         jerboa v0.60.0", lines[0])
	assert.Equal(t, "[warn] disk space  only 3.0GB free", lines[1])
	assert.Equal(t, "                   -> jerboa system prune", lines[2])
	assert.Equal(t, "[FAIL] images      image web:latest is corrupt", lines[3])
	assert.Equal(t, "                   -> jerboa rmi web:latest", lines[4])
	assert.Equal(t, "[info] kernel      not downloaded yet", lines[5])
	assert.Equal(t, "[novel] x           from a newer daemon", lines[6])
	assert.Contains(t, out.String(), "1 problem(s), 1 warning(s).")
}

func TestRenderDoctor_AllGood(t *testing.T) {
	var out bytes.Buffer
	renderDoctor(&out, []api.SystemFinding{
		{Check: "cli", Severity: api.SeverityOK, Message: "fine"},
		{Check: "kernel", Severity: api.SeverityInfo, Message: "not downloaded yet"},
	})
	assert.Contains(t, out.String(), "Everything looks right.", "information is not a problem")
}

func TestCountSeverity(t *testing.T) {
	findings := []api.SystemFinding{{Severity: "ok"}, {Severity: "error"}, {Severity: "error"}, {Severity: "warning"}}
	assert.Equal(t, 2, countSeverity(findings, api.SeverityError))
	assert.Equal(t, 1, countSeverity(findings, api.SeverityWarning))
	assert.Zero(t, countSeverity(nil, api.SeverityError))
}

func TestDefaultDoctorEnv_ReadsThisMachine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	env := defaultDoctorEnv()
	assert.NotEmpty(t, env.goos)
	assert.NotEmpty(t, env.configPath)
	require.NoError(t, env.loadConfig(env.configPath), "no config file is a valid configuration")
	_, _, exists, err := env.credentials()
	require.NoError(t, err)
	assert.False(t, exists)
	assert.NotPanics(t, func() { env.kvmPresent() })
}

// The doctor runs to the end and says so when there is no daemon at all.
func TestDoctor_NoDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("JERBOA_HOST", "")
	t.Setenv("JERBOA_AUTH_TOKEN", "")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--host", "tcp://127.0.0.1:1", "doctor"})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "doctor: 1 problem(s) found")
	assert.Contains(t, out.String(), "[ ok ] cli")
	assert.Contains(t, out.String(), "[FAIL] daemon")
	assert.Contains(t, out.String(), "not reachable")

	out.Reset()
	root = newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--host", "tcp://127.0.0.1:1", "--output", "json", "doctor"})
	require.Error(t, root.Execute())
	assert.Contains(t, out.String(), `"ok": false`)
	assert.Contains(t, out.String(), `"check": "daemon"`)
}
