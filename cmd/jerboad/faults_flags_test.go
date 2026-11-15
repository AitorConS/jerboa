//go:build linux || (darwin && arm64)

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about what the daemon refuses to start with: a token file
// that is missing, empty or readable by others, an endpoint that would be
// served without authentication, flags that do not apply to this host. Each
// refusal must name what is wrong, and must happen before anything is served.

// strongToken is long enough for a TCP endpoint.
const strongToken = "0123456789abcdef0123456789abcdef"

// passedMarker is a --native-peering value nobody would use. That flag is the
// last thing checked before the daemon starts serving, so a command line that
// passes every earlier check is rejected for it, and never serves.
const passedMarker = "validation-passed"

// validate runs jerboad's command line against a private home directory and
// returns what startup failed with. No daemon is ever started: see
// passedMarker.
func validate(t *testing.T, args ...string) error {
	t.Helper()
	isolateHome(t)
	return execute(t, args...)
}

// execute is validate without touching the environment, for tests that set
// tokens in it themselves.
func execute(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceUsage = true
	cmd.SetArgs(append(args, "--native-peering", passedMarker))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := cmd.ExecuteContext(ctx)
	require.Error(t, err, "the command line must never get as far as serving")
	return err
}

// requirePassed asserts that every check before serving accepted the command line.
func requirePassed(t *testing.T, err error) {
	t.Helper()
	require.ErrorContains(t, err, passedMarker)
}

// requireRefused asserts startup failed for the reason given, and not merely
// at the final marker.
func requireRefused(t *testing.T, err error, want string) {
	t.Helper()
	require.ErrorContains(t, err, want)
	require.NotContains(t, err.Error(), passedMarker)
}

func secretFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("JERBOA_AUTH_TOKEN", "")
	t.Setenv("JERBOA_CLUSTER_TOKEN", "")
	t.Setenv("JERBOA_OBSERVABILITY_TOKEN", "")
}

func TestFaults_TCPEndpointNeedsAToken(t *testing.T) {
	// Any local process, and off loopback any host, reaches a TCP endpoint.
	for _, args := range [][]string{
		{"--host", "tcp://127.0.0.1:0"},
		{"-H", "tcp://0.0.0.0:7890"},
		// The deprecated flag takes the same endpoints and gets no exemption.
		{"--socket", "tcp://127.0.0.1:0"},
	} {
		err := validate(t, args...)
		requireRefused(t, err, "refusing to serve TCP endpoint")
		assert.Contains(t, err.Error(), "--insecure", "the refusal says how to override it")
	}

	requirePassed(t, validate(t, "--host", "tcp://127.0.0.1:0", "--insecure"))
	requirePassed(t, validate(t, "--host", "unix:///tmp/never-bound.sock"))
}

func TestFaults_TCPEndpointRefusesAGuessableToken(t *testing.T) {
	isolateHome(t)
	t.Setenv("JERBOA_AUTH_TOKEN", "hunter2")
	err := execute(t, "--host", "tcp://127.0.0.1:0")
	requireRefused(t, err, "too short")
	assert.NotContains(t, err.Error(), "hunter2", "the token itself must not reach the error or the log")

	// A Unix socket is limited to its owner by file permissions.
	requirePassed(t, execute(t, "--host", "unix:///tmp/never-bound.sock"))

	t.Setenv("JERBOA_AUTH_TOKEN", strongToken)
	requirePassed(t, execute(t, "--host", "tcp://127.0.0.1:0"))
}

func TestFaults_TokenFileProblemsStopStartup(t *testing.T) {
	dir := t.TempDir()
	linkTarget := secretFile(t, strongToken, 0o644)
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(linkTarget, link))

	cases := []struct {
		name, path, want string
		needsNonRoot     bool
	}{
		{name: "missing", path: filepath.Join(dir, "absent"), want: "no such file"},
		{name: "world readable", path: secretFile(t, strongToken, 0o644), want: "readable by other users"},
		{name: "group readable", path: secretFile(t, strongToken, 0o640), want: "readable by other users"},
		{name: "empty", path: secretFile(t, "", 0o600), want: "is empty"},
		{name: "only whitespace", path: secretFile(t, " \n\t\r\n", 0o600), want: "is empty"},
		{name: "a directory", path: t.TempDir(), want: "jerboad:"},
		{name: "link to a readable file", path: link, want: "readable by other users"},
		{name: "unreadable", path: secretFile(t, strongToken, 0o000), want: "permission denied", needsNonRoot: true},
	}
	flags := []struct{ flag, env string }{
		{"--auth-token-file", "JERBOA_AUTH_TOKEN"},
		{"--cluster-token-file", "JERBOA_CLUSTER_TOKEN"},
		{"--observability-token-file", "JERBOA_OBSERVABILITY_TOKEN"},
	}
	for _, f := range flags {
		for _, tc := range cases {
			t.Run(f.flag+"/"+tc.name, func(t *testing.T) {
				if tc.needsNonRoot && os.Geteuid() == 0 {
					t.Skip("root reads any file")
				}
				isolateHome(t)
				// A usable token in the environment must not paper over a
				// file the operator named and that cannot be used.
				t.Setenv(f.env, strongToken)
				err := execute(t, "--host", "tcp://127.0.0.1:0", "--insecure", f.flag, tc.path)
				requireRefused(t, err, tc.want)
				assert.NotContains(t, err.Error(), strongToken)
			})
		}
	}
}

func TestFaults_TokenFileIsTrimmedAndBeatsTheEnvironment(t *testing.T) {
	isolateHome(t)
	// An editor's trailing newline, or a file written on Windows, is not part
	// of the secret: the token still counts as its 32 characters.
	file := secretFile(t, strongToken+"\r\n", 0o600)
	requirePassed(t, execute(t, "--host", "tcp://127.0.0.1:0", "--auth-token-file", file))

	// A short token in the file is what is used, not the strong one in the
	// environment.
	t.Setenv("JERBOA_AUTH_TOKEN", strongToken)
	short := secretFile(t, "short\n", 0o600)
	requireRefused(t, execute(t, "--host", "tcp://127.0.0.1:0", "--auth-token-file", short), "too short (5 characters")
}

func TestFaults_AuxiliaryEndpointsNeedTokens(t *testing.T) {
	sock := []string{"--host", "unix:///tmp/never-bound.sock"}
	with := func(extra ...string) []string { return append(append([]string{}, sock...), extra...) }

	requireRefused(t, validate(t, with("--ui-addr", "127.0.0.1:8080")...), "--ui-addr")
	requireRefused(t, validate(t, with("--metrics-addr", ":9090")...), "--metrics-addr")
	requireRefused(t, validate(t, with("--metrics-addr", "192.168.1.10:9090")...), "off loopback")
	requireRefused(t, validate(t, with("--cluster-addr", ":7946")...), "cluster gossip")
	// --insecure covers the API, metrics and the dashboard; gossip accepts
	// members, and has its own switch.
	requireRefused(t, validate(t, with("--cluster-addr", ":7946", "--insecure")...), "--insecure-cluster")

	requirePassed(t, validate(t, with("--metrics-addr", "127.0.0.1:9090")...))
	requirePassed(t, validate(t, with("--ui-addr", "127.0.0.1:8080", "--insecure")...))
	requirePassed(t, validate(t, with("--cluster-addr", ":7946", "--insecure-cluster")...))

	obs := secretFile(t, "scrape-token\n", 0o600)
	requirePassed(t, validate(t, with("--ui-addr", ":8080", "--metrics-addr", ":9090", "--observability-token-file", obs)...))
	cluster := secretFile(t, "gossip-token\n", 0o600)
	requirePassed(t, validate(t, with("--cluster-addr", ":7946", "--cluster-token-file", cluster)...))
}

func TestFaults_ObservabilityTokenDoesNotOpenTheAPI(t *testing.T) {
	// The scrape token is read-only by design: holding it must not count as
	// authentication for a TCP API endpoint.
	isolateHome(t)
	t.Setenv("JERBOA_OBSERVABILITY_TOKEN", strongToken)
	t.Setenv("JERBOA_CLUSTER_TOKEN", strongToken)
	requireRefused(t, execute(t, "--host", "tcp://127.0.0.1:0"), "refusing to serve TCP endpoint")
}

func TestFaults_FirecrackerSecurityPolicy(t *testing.T) {
	sock := []string{"--host", "unix:///tmp/never-bound.sock"}
	policy := func(content string) string {
		path := filepath.Join(t.TempDir(), "policy.json")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	if runtime.GOOS != "darwin" {
		err := validate(t, append(sock, "--fc-security", policy(`{}`))...)
		requireRefused(t, err, "only supported on macOS")
		return
	}
	requireRefused(t, validate(t, append(sock, "--fc-security", filepath.Join(t.TempDir(), "absent.json"))...),
		"read Firecracker security policy")
	for name, content := range map[string]string{
		"empty":     "",
		"truncated": `{"egress": [{"cidr": "0.0.0.0/0"`,
		"yaml":      "egress:\n  - cidr: 0.0.0.0/0\n",
		"binary":    "\x00\x01\xff",
		"two docs":  `{} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			// A policy that does not parse must not fall back to the default
			// one, which is more permissive than what the operator intended.
			requireRefused(t, validate(t, append(sock, "--fc-security", policy(content))...), "must be valid JSON")
		})
	}
}

func TestFaults_HostSpecificFlagsAreRefusedElsewhere(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("--fc-metrics-dir is a Linux flag")
	}
	err := validate(t, "--host", "unix:///tmp/never-bound.sock", "--fc-metrics-dir", t.TempDir())
	requireRefused(t, err, "only supported on Linux")
}

func TestFaults_InvalidFlagValues(t *testing.T) {
	isolateHome(t)
	run := func(args ...string) error {
		cmd := newRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return cmd.ExecuteContext(ctx)
	}
	sock := "unix:///tmp/never-bound.sock"

	err := run("--host", sock, "--native-peering", "on")
	require.ErrorContains(t, err, `--native-peering "on" is invalid`)
	assert.Contains(t, err.Error(), "auto or off")

	for _, args := range [][]string{
		{"--host", sock, "--snapshot-max-count", "many"},
		{"--host", sock, "--snapshot-max-bytes", "32G"},
		{"--host", sock, "--vm-log-max-bytes", "4MiB"},
		{"--host", sock, "--insecure=maybe"},
		{"--host", sock, "--no-such-flag"},
		{"--host"},
	} {
		err := run(args...)
		require.Error(t, err, "%v", args)
		assert.NotContains(t, err.Error(), "goroutine", "%v", args)
		assert.Less(t, strings.Count(err.Error(), "\n"), 3, "one short message, not a dump: %q", err.Error())
	}
}

func TestFaults_ResolveSecretPrecedence(t *testing.T) {
	t.Setenv("JERBOA_TEST_SECRET", "from-env")
	file := secretFile(t, "from-file\n", 0o600)

	got, err := resolveSecret("from-flag", file, "JERBOA_TEST_SECRET")
	require.NoError(t, err)
	assert.Equal(t, "from-flag", got)

	got, err = resolveSecret("", file, "JERBOA_TEST_SECRET")
	require.NoError(t, err)
	assert.Equal(t, "from-file", got)

	// Nothing configured anywhere is "no authentication", not an error: the
	// caller decides whether the endpoint may be served that way.
	t.Setenv("JERBOA_TEST_SECRET", "")
	got, err = resolveSecret("", "", "JERBOA_TEST_SECRET")
	require.NoError(t, err)
	assert.Empty(t, got)

	// The error for an over-shared file says how to fix it.
	loose := secretFile(t, "x", 0o604)
	_, err = resolveSecret("", loose, "JERBOA_TEST_SECRET")
	require.ErrorContains(t, err, "chmod 600 "+loose)
	assert.Contains(t, err.Error(), "0604")
}

func TestFaults_TokenStrengthBoundary(t *testing.T) {
	tcp := "tcp://127.0.0.1:7890"
	require.Error(t, checkTokenStrength(tcp, strings.Repeat("a", minTCPTokenLength-1), false))
	require.NoError(t, checkTokenStrength(tcp, strings.Repeat("a", minTCPTokenLength), false))
	// No token at all is a different refusal, made by the caller.
	require.NoError(t, checkTokenStrength(tcp, "", false))

	err := checkTokenStrength(tcp, "s3cret", false)
	require.ErrorContains(t, err, "6 characters")
	assert.NotContains(t, err.Error(), "s3cret")
	assert.Contains(t, err.Error(), "openssl rand -hex 32", "the refusal says how to make a good one")
}

func TestFaults_SplitCommaListDropsBlanks(t *testing.T) {
	// --join is typed by hand: stray commas and spaces must not become seed
	// addresses the daemon then tries to dial.
	assert.Equal(t, []string{"10.0.0.2:7946", "10.0.0.3:7946"}, splitCommaList(" 10.0.0.2:7946, ,10.0.0.3:7946,, "))
	assert.Empty(t, splitCommaList(""))
	assert.Empty(t, splitCommaList(" , ,"))
}
