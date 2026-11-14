//go:build !windows

package wslboot

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover what goes wrong around bringing the daemon up inside
// WSL: wsl.exe is missing or fails, the daemon never becomes reachable, what
// answers on the endpoint is not a healthy daemon, and the file holding the
// daemon's token is damaged or cannot be written. They also check that no
// value taken from configuration can break out of the shell command the
// daemon is launched with.

const faultWait = 10 * time.Second

// isolateHome keeps the launch log out of the real home directory.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// fakeWSL puts a wsl command first on PATH. It records its arguments and the
// token variables it was given, prints output and exits with code.
func fakeWSL(t *testing.T, output, code string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + dir + "/args'\n" +
		"printf '%s|%s|%s' \"$JERBOA_AUTH_TOKEN\" \"$JERBOA_OBSERVABILITY_TOKEN\" \"$WSLENV\" > '" + dir + "/env'\n" +
		"printf '%s' '" + output + "'\n" +
		"exit " + code + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wsl"), []byte(script), 0o700)) //nolint:gosec // the test's own script must be executable
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	return dir
}

func readEventually(t *testing.T, path string) string {
	t.Helper()
	var data []byte
	require.Eventually(t, func() bool {
		var err error
		data, err = os.ReadFile(path)
		return err == nil && len(data) > 0
	}, faultWait, 5*time.Millisecond, "%s was never written", path)
	return string(data)
}

// --- stopping ---

func TestFaults_Stop(t *testing.T) {
	t.Run("no daemon running", func(t *testing.T) {
		fakeWSL(t, "", "1")
		require.ErrorIs(t, Stop("jerboa", "root"), ErrNoDaemon)
	})
	t.Run("wsl fails", func(t *testing.T) {
		fakeWSL(t, "There is no distribution with the supplied name.", "255")
		err := Stop("jerboa", "root")
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNoDaemon, "a WSL failure was reported as 'nothing to stop'")
		assert.Contains(t, err.Error(), "There is no distribution with the supplied name.")
	})
	t.Run("wsl missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		err := Stop("jerboa", "root")
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNoDaemon)
		assert.Contains(t, err.Error(), "wslboot: stop daemon")
	})
	t.Run("stops only jerboad in the named distro", func(t *testing.T) {
		dir := fakeWSL(t, "", "0")
		require.NoError(t, Stop("jerboa", "root"))
		assert.Equal(t, "-d\njerboa\n-u\nroot\n--\nbash\n-lc\npkill -f 'jerboad --host'\n", readEventually(t, filepath.Join(dir, "args")))

		dir = fakeWSL(t, "", "0")
		require.NoError(t, Stop("", ""))
		assert.Equal(t, "--\nbash\n-lc\npkill -f 'jerboad --host'\n", readEventually(t, filepath.Join(dir, "args")))
	})
}

// --- launching ---

func TestFaults_LaunchWithoutWSL(t *testing.T) {
	isolateHome(t)
	t.Setenv("PATH", t.TempDir())
	err := Launch(Config{Endpoint: "tcp://127.0.0.1:1", Distro: "jerboa"})
	require.ErrorContains(t, err, "wslboot: start daemon")
	assert.Contains(t, err.Error(), "wsl")

	err = EnsureDaemon(context.Background(), Config{Endpoint: "tcp://127.0.0.1:1", HealthTimeout: time.Second})
	require.ErrorContains(t, err, "wslboot: start daemon", "an unreachable daemon that cannot be started must fail at once, not after the health timeout")
}

// The token reaches the distro through the environment only: on a command
// line every process of the machine could read it.
func TestFaults_LaunchKeepsTheTokenOffTheCommandLine(t *testing.T) {
	home := isolateHome(t)
	dir := fakeWSL(t, "", "0")
	const token, obsToken = "launch-secret-token-0123456789", "observability-secret-0123456789"
	require.NoError(t, Launch(Config{
		Endpoint: "tcp://172.20.0.2:7890", ListenEndpoint: "tcp://0.0.0.0:7890", Distro: "jerboa", User: "root",
		Token: token, Observability: Observability{MetricsAddr: "127.0.0.1:9100", Token: obsToken},
	}))
	args := readEventually(t, filepath.Join(dir, "args"))
	assert.NotContains(t, args, token)
	assert.NotContains(t, args, obsToken)
	assert.Contains(t, args, "--host 'tcp://0.0.0.0:7890'", "the daemon must listen where it was told, not where the client dials")
	assert.NotContains(t, args, "172.20.0.2")
	assert.Equal(t, token+"|"+obsToken+"|JERBOA_AUTH_TOKEN/u:JERBOA_OBSERVABILITY_TOKEN/u", readEventually(t, filepath.Join(dir, "env")))

	info, err := os.Stat(filepath.Join(home, ".jerboa", "jerboad-wsl.log"))
	require.NoError(t, err, "the launch log must be under the home directory in use")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// The launch still happens when the launch log cannot be opened; the daemon
// then logs to /dev/null instead of failing to start.
func TestFaults_LaunchWithUnusableLogDirectory(t *testing.T) {
	home := isolateHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".jerboa"), []byte("a file where the directory should be"), 0o600))
	dir := fakeWSL(t, "", "0")
	require.NoError(t, Launch(Config{Endpoint: "tcp://127.0.0.1:1"}))
	assert.Contains(t, readEventually(t, filepath.Join(dir, "args")), ">> '/dev/null'")
}

// wsl starts but no daemon ever answers: the wait ends with an error that
// names the endpoint, at the timeout or as soon as the caller gives up.
func TestFaults_DaemonNeverBecomesHealthy(t *testing.T) {
	isolateHome(t)
	fakeWSL(t, "", "0")
	err := EnsureDaemon(context.Background(), Config{Endpoint: "tcp://127.0.0.1:1", HealthTimeout: 300 * time.Millisecond})
	require.ErrorContains(t, err, "did not become reachable at tcp://127.0.0.1:1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = WaitHealthy(ctx, Config{Endpoint: "tcp://127.0.0.1:1", HealthTimeout: time.Hour})
	require.ErrorIs(t, err, context.Canceled, "a canceled wait must not run to its timeout")
}

// serveOnce accepts connections and hands each to handle.
func serveOnce(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // test listener
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				handle(conn)
			}()
		}
	}()
	return "tcp://" + ln.Addr().String()
}

// Something listens on the endpoint but is not a daemon that answers. It
// must not count as healthy, or the CLI would skip starting the real one.
func TestFaults_UnhealthyListeners(t *testing.T) {
	readLine := func(conn net.Conn) { _, _ = conn.Read(make([]byte, 4096)) }
	listeners := map[string]func(net.Conn){
		"closes at once":  func(net.Conn) {},
		"closes mid-word": func(c net.Conn) { readLine(c); _, _ = c.Write([]byte(`{"jsonrpc":"2.0","id":1,"resu`)) },
		"speaks http":     func(c net.Conn) { readLine(c); _, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n")) },
		"answers an error": func(c net.Conn) {
			readLine(c)
			_, _ = c.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"authentication required"}}` + "\n"))
		},
	}
	for name, handle := range listeners {
		t.Run(name, func(t *testing.T) {
			endpoint := serveOnce(t, handle)
			assert.False(t, Healthy(context.Background(), endpoint, ""))
			assert.False(t, Healthy(context.Background(), endpoint, "some-token-0123456789abcdef"))
		})
	}
	for _, endpoint := range []string{"", "http://127.0.0.1:1", "tcp://", "tcp://127.0.0.1:notaport", "unix:///nonexistent/dir/d.sock"} {
		assert.False(t, Healthy(context.Background(), endpoint, ""), "endpoint %q", endpoint)
	}
}

// --- shell quoting ---

var hostileValues = []string{
	"plain",
	"with space",
	"it's",
	"''",
	"'; touch INJECTED; '",
	"$(touch INJECTED)",
	"`touch INJECTED`",
	"a\"b",
	"a\\b",
	"a\nb",
	"*",
	"~",
	"-n",
	"&& touch INJECTED",
	"| touch INJECTED",
	"> INJECTED",
	"$HOME",
	"tcp://0.0.0.0:7890'; touch INJECTED #",
	"émoji ☃",
	"",
}

// A quoted value is one shell word, whatever it holds.
func TestFaults_ShQuoteIsOneWord(t *testing.T) {
	for _, value := range hostileValues {
		dir := t.TempDir()
		cmd := exec.Command("sh", "-c", "printf '%s' "+shQuote(value)) //nolint:gosec,noctx // the quoting is what is under test
		cmd.Dir = dir
		out, err := cmd.Output()
		require.NoError(t, err, "value %q", value)
		assert.Equal(t, value, string(out))
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries, "value %q ran a command", value)
	}
}

// Every configurable value of the launch command reaches jerboad as exactly
// the argument it was, when the command line is run by a real shell.
func TestFaults_LaunchCommandResistsInjection(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("needs bash")
	}
	for _, value := range hostileValues {
		if value == "" {
			continue // an empty value means "not set" and adds no argument
		}
		t.Run(value, func(t *testing.T) {
			work := t.TempDir()
			bin := t.TempDir()
			// setsid stands in for the real one and records what it is asked
			// to run, one argument per NUL-terminated record.
			recorder := "#!/bin/sh\nprintf '%s\\0' \"$@\" > '" + work + "/argv.tmp' && mv '" + work + "/argv.tmp' '" + work + "/argv'\n"
			require.NoError(t, os.WriteFile(filepath.Join(bin, "setsid"), []byte(recorder), 0o700)) //nolint:gosec // the test's own script must be executable
			logPath := filepath.Join(work, "log "+strings.ReplaceAll(value, "/", "_"))
			args, _ := buildLaunchArgs(Config{
				Endpoint: value, JerboadPath: value, Hypervisor: value,
				Observability: Observability{MetricsAddr: value, UIAddr: value, TraceAddr: value, LogFormat: value},
			}, logPath)
			inner := args[len(args)-1]

			cmd := exec.Command("bash", "-c", inner) //nolint:gosec,noctx // the generated command is what is under test
			cmd.Dir = work
			cmd.Env = []string{"PATH=" + bin + ":/bin:/usr/bin", "HOME=" + work}
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "output: %s", out)

			want := strings.Join([]string{value, "--host", value, "--hypervisor", value, "--metrics-addr", value, "--ui-addr", value, "--trace-addr", value, "--log-format", value}, "\x00") + "\x00"
			assert.Equal(t, want, readEventually(t, filepath.Join(work, "argv")))
			_, err = os.Stat(filepath.Join(work, "INJECTED"))
			require.ErrorIs(t, err, os.ErrNotExist, "the value ran a command")
		})
	}
}

// --- the daemon rendezvous file ---

// A damaged daemon.json is an error that names the file. It is not taken for
// "no token yet": generating a new token would lock the client out of the
// daemon that is running with the old one.
func TestFaults_DamagedDaemonFile(t *testing.T) {
	contents := map[string]string{
		"truncated":  `{"token":"abc`,
		"empty":      ``,
		"garbage":    "\x00\x01\x02",
		"wrong type": `{"token":["a"],"endpoint":7}`,
		"array":      `["token"]`,
	}
	for name, content := range contents {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.json")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			token, endpoint, err := LoadDaemonFile(path)
			require.ErrorContains(t, err, path)
			assert.Empty(t, token)
			assert.Empty(t, endpoint)

			token, err = LoadOrCreateToken(path)
			require.ErrorContains(t, err, path)
			assert.Empty(t, token)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, content, string(data), "a damaged file was overwritten")
		})
	}
}

func TestFaults_UnreadableDaemonFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	path := filepath.Join(t.TempDir(), "daemon.json")
	require.NoError(t, SaveDaemonFile(path, "the-token", "tcp://10.0.0.1:7890"))
	require.NoError(t, os.Chmod(path, 0o000))

	_, _, err := LoadDaemonFile(path)
	require.ErrorContains(t, err, path)
	_, err = LoadOrCreateToken(path)
	require.Error(t, err, "an unreadable token file was replaced by a new token")

	require.NoError(t, os.Chmod(path, 0o600))
	token, endpoint, err := LoadDaemonFile(path)
	require.NoError(t, err)
	assert.Equal(t, "the-token", token)
	assert.Equal(t, "tcp://10.0.0.1:7890", endpoint)
}

// A file without a token (written by hand, or by an older version) gets one,
// and the token then stays the same on every later call.
func TestFaults_DaemonFileWithoutToken(t *testing.T) {
	for name, content := range map[string]string{"empty object": `{}`, "empty token": `{"token":""}`, "null": `null`} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.json")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			token, err := LoadOrCreateToken(path)
			require.NoError(t, err)
			assert.Len(t, token, 64)
			again, err := LoadOrCreateToken(path)
			require.NoError(t, err)
			assert.Equal(t, token, again)
		})
	}
	t.Run("tokens differ between machines", func(t *testing.T) {
		a, err := LoadOrCreateToken(filepath.Join(t.TempDir(), "daemon.json"))
		require.NoError(t, err)
		b, err := LoadOrCreateToken(filepath.Join(t.TempDir(), "daemon.json"))
		require.NoError(t, err)
		assert.NotEqual(t, a, b)
	})
}

func TestFaults_DaemonFileDirectoryUnusable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	path := filepath.Join(blocker, ".jerboa", "daemon.json")

	require.ErrorContains(t, SaveDaemonFile(path, "t", "e"), "wslboot:")
	token, err := LoadOrCreateToken(path)
	require.Error(t, err)
	assert.Empty(t, token, "a token that could not be stored must not be handed out: the next run would generate another")
}

// The disk fills up, errors, or the process dies while the rendezvous file
// is written. The file must then hold the old token or the new one, whole;
// never half of one, and never nothing.
func TestFaults_SaveDaemonFileFailures(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".jerboa", "daemon.json")
			require.NoError(t, SaveDaemonFile(path, "old-token", "tcp://10.0.0.1:7890"))

			inj := durabletest.Inject(t, f, 0, nil)
			err := SaveDaemonFile(path, "new-token", "tcp://10.0.0.2:7890")
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			token, endpoint, lerr := LoadDaemonFile(path)
			require.NoError(t, lerr, "a failed save left a rendezvous file that does not parse")
			if err != nil && f.Step != durable.StepSyncDir {
				assert.Equal(t, "old-token", token)
				assert.Equal(t, "tcp://10.0.0.1:7890", endpoint)
			} else {
				assert.Equal(t, "new-token", token)
				assert.Equal(t, "tcp://10.0.0.2:7890", endpoint)
			}
			if !f.Crash() {
				entries, rerr := os.ReadDir(filepath.Dir(path))
				require.NoError(t, rerr)
				assert.Len(t, entries, 1, "a failed write left temporary files behind")
			}
		})
	}
}

// The first token cannot be stored. The caller gets an error, not a token
// that only this process knows; the next attempt starts clean and succeeds.
func TestFaults_FirstTokenWriteFails(t *testing.T) {
	for _, f := range durabletest.Failures() {
		if f.Step == durable.StepSyncDir && !f.Crash() {
			continue // the file is in place; only the directory sync was lost
		}
		t.Run(f.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".jerboa", "daemon.json")
			inj := durabletest.Inject(t, f, 0, nil)
			token, err := LoadOrCreateToken(path)
			require.True(t, inj.Fired())
			durable.SetFault(nil)
			require.Error(t, err)
			assert.Empty(t, token)
			if !f.Crash() {
				require.NotErrorIs(t, err, durable.ErrCrash)
			}

			next, err := LoadOrCreateToken(path)
			require.NoError(t, err, "a failed first write must not block the next one")
			stored, _, err := LoadDaemonFile(path)
			require.NoError(t, err)
			assert.Equal(t, next, stored)
		})
	}
}
