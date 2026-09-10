// Package wslboot bootstraps the jerboad daemon inside WSL2 from the Windows
// client. The daemon always runs on Linux; on Windows jerboa.exe is a thin client
// that health-checks a loopback TCP endpoint and, if nothing answers, launches
// jerboad inside a WSL2 distribution and waits for it to come up.
package wslboot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
)

// ErrNoDaemon reports that no running daemon was found to act on (e.g. by Stop).
var ErrNoDaemon = errors.New("no running daemon found")

// Config describes how to reach and, if needed, launch the daemon.
type Config struct {
	// Endpoint is the address the client dials, e.g. tcp://127.0.0.1:7890.
	Endpoint string
	// ListenEndpoint is the --host value the daemon binds. Empty reuses Endpoint.
	// Used to bind 0.0.0.0 inside a dedicated distro while the client dials
	// loopback across the WSL2 boundary.
	ListenEndpoint string
	// Distro is the WSL2 distribution name. Empty uses the WSL default distro.
	Distro string
	// User is the Linux user to run jerboad as inside the distro (wsl -u). Empty
	// uses the distro's default user. "root" runs privileged without host sudo.
	User string
	// Token is the shared secret passed to the daemon (via the environment, not
	// argv) and used by the client handshake. Empty disables authentication.
	Token string
	// JerboadPath is the jerboad binary path inside WSL. Empty resolves "jerboad"
	// on the distro's PATH.
	JerboadPath string
	// Hypervisor, when non-empty, is passed to jerboad as --hypervisor (e.g.
	// "firecracker"). Empty leaves the daemon's own default.
	Hypervisor string
	// Sudo runs jerboad under sudo inside the distro. Required for hypervisors
	// that need privileges (firecracker networking). The token is forwarded with
	// sudo --preserve-env so it never appears on the command line.
	Sudo bool
	// HealthTimeout bounds how long to wait for a freshly launched daemon to
	// answer. Zero defaults to 20s.
	HealthTimeout time.Duration
	// Observability turns on the managed daemon's metrics/UI/tracing/log-format,
	// which are otherwise reachable only by hand-launching jerboad inside the
	// distro. Persisted in config so both `daemon start` and
	// the auto-boot path enable them.
	Observability Observability
}

// Observability holds jerboad flags forwarded verbatim to enable the daemon's
// metrics endpoint, dashboard, OTLP trace export, and structured logging. Empty
// fields are omitted so the daemon keeps its own defaults.
type Observability struct {
	MetricsAddr string
	UIAddr      string
	TraceAddr   string
	LogFormat   string
}

// EnsureDaemon returns nil once the daemon is reachable. If it is not already
// answering, the daemon is started inside WSL2 and EnsureDaemon waits for it.
func EnsureDaemon(ctx context.Context, cfg Config) error {
	if healthy(ctx, cfg.Endpoint, cfg.Token) {
		return nil
	}
	if err := launchInWSL(cfg); err != nil {
		return fmt.Errorf("wslboot: start daemon: %w", err)
	}
	return waitHealthy(ctx, cfg)
}

// Launch starts jerboad inside WSL2 detached, per cfg, without first checking
// whether one is already running. Exposed for `jerboa daemon start`; EnsureDaemon
// uses the same machinery for auto-boot.
func Launch(cfg Config) error {
	if err := launchInWSL(cfg); err != nil {
		return fmt.Errorf("wslboot: start daemon: %w", err)
	}
	return nil
}

// WaitHealthy polls cfg.Endpoint until the daemon answers or the timeout fires.
func WaitHealthy(ctx context.Context, cfg Config) error { return waitHealthy(ctx, cfg) }

// Healthy reports whether a daemon answers (and authenticates) at endpoint.
func Healthy(ctx context.Context, endpoint, token string) bool {
	return healthy(ctx, endpoint, token)
}

// Stop terminates the jerboad daemon running inside the WSL2 distro. distro
// empty targets the default distro; user (e.g. "root") selects the wsl user that
// can signal it. Returns ErrNoDaemon when nothing matched.
func Stop(distro, user string) error {
	var args []string
	if distro != "" {
		args = append(args, "-d", distro)
	}
	if user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, "--", "bash", "-lc", "pkill -f 'jerboad --host'")
	out, err := exec.Command("wsl", args...).CombinedOutput() //nolint:gosec,noctx // fixed program, controlled args
	if err != nil {
		// pkill exits 1 when no process matched: treat as "already stopped".
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return ErrNoDaemon
		}
		return fmt.Errorf("wslboot: stop daemon: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LoadDaemonFile reads the token and endpoint persisted at path. A missing file
// returns empty strings and a nil error.
func LoadDaemonFile(path string) (token, endpoint string, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("wslboot: read %s: %w", path, err)
	}
	var f daemonFile
	if uerr := json.Unmarshal(data, &f); uerr != nil {
		return "", "", fmt.Errorf("wslboot: parse %s: %w", path, uerr)
	}
	return f.Token, f.Endpoint, nil
}

// SaveDaemonFile persists the daemon's token and endpoint to path (mode 0600) so
// every client run reaches the same daemon with the same secret.
func SaveDaemonFile(path, token, endpoint string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("wslboot: create dir: %w", err)
	}
	out, err := json.MarshalIndent(daemonFile{Token: token, Endpoint: endpoint}, "", "  ")
	if err != nil {
		return fmt.Errorf("wslboot: marshal daemon file: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("wslboot: write %s: %w", path, err)
	}
	return nil
}

// healthy reports whether a daemon answers (and authenticates) at endpoint.
func healthy(ctx context.Context, endpoint, token string) bool {
	c, err := api.DialWithToken(endpoint, token)
	if err != nil {
		return false
	}
	defer func() { _ = c.Close() }()
	_, err = c.DaemonVersion(ctx)
	return err == nil
}

// waitHealthy polls the endpoint until the daemon answers or the timeout fires.
func waitHealthy(ctx context.Context, cfg Config) error {
	timeout := cfg.HealthTimeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if healthy(ctx, cfg.Endpoint, cfg.Token) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wslboot: daemon did not become reachable at %s within %s", cfg.Endpoint, timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wslboot: wait for daemon: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// launchInWSL starts jerboad inside the configured WSL distro, daemonised
// *inside* the distro with `setsid` (mirrored by JerboaDesktop's launchDaemon):
//   - setsid puts jerboad in a new session with no controlling terminal, so it
//     survives the one-shot `wsl -- ...` invocation returning (a plain `&` job
//     would be reaped on session teardown) and the launching wsl.exe exits
//     within a second instead of living forever.
//   - That matters on Windows 11 with Windows Terminal as the default terminal:
//     a long-lived foreground wsl.exe hijacks a terminal tab as its console. A
//     short, non-interactive invocation (stdin from /dev/null, output redirected
//     in-distro) leaves no window — the daemon runs silently in the background
//     like Docker's engine.
//   - jerboad's stdout/stderr go to ~/.jerboa/jerboad-wsl.log via the distro's
//     /mnt view of that Windows path; the short-lived wsl.exe's own output
//     (e.g. "distro not found") is captured into the same log from this side.
//
// The auth token is supplied through the environment, never argv.
func launchInWSL(cfg Config) error {
	wslLog := "/dev/null"
	logf, logErr := openLaunchLog()
	if logErr == nil {
		wslLog = toWSLPath(logf.Name())
	}
	args, env := buildLaunchArgs(cfg, wslLog)
	cmd := exec.Command("wsl", args...) //nolint:gosec,noctx // fixed program, controlled args; must outlive caller
	cmd.Env = env
	cmd.SysProcAttr = detachAttr()
	if logErr == nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("wsl launch: %w", err)
	}
	// Do not Wait: launch failures surface through the health check and the
	// launch log. Releasing the handle lets the OS reap the short-lived wsl.exe
	// independently of this client invocation.
	_ = cmd.Process.Release()
	return nil
}

// shQuote wraps a value in single quotes for a POSIX shell, escaping any
// embedded single quotes — used to build the in-distro launch command.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// toWSLPath translates a Windows absolute path to its WSL2 equivalent so the
// distro can open it via the /mnt/<drive>/... mount point.
// Example: C:\Users\foo\bar → /mnt/c/Users/foo/bar
func toWSLPath(p string) string {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		drive := strings.ToLower(string(p[0]))
		rest := strings.ReplaceAll(p[3:], "\\", "/")
		return "/mnt/" + drive + "/" + rest
	}
	return strings.ReplaceAll(p, "\\", "/")
}

// buildLaunchArgs builds the `wsl` arguments and process environment that start
// jerboad as a detached background process inside the distro, logging to
// wslLog (a path as seen from inside the distro). Exposed (unexported) for
// testing.
func buildLaunchArgs(cfg Config, wslLog string) (args, env []string) {
	jerboad := cfg.JerboadPath
	if jerboad == "" {
		jerboad = "jerboad"
	}
	if cfg.Distro != "" {
		args = append(args, "-d", cfg.Distro)
	}
	if cfg.User != "" {
		args = append(args, "-u", cfg.User)
	}

	listen := cfg.ListenEndpoint
	if listen == "" {
		listen = cfg.Endpoint
	}
	inner := "setsid "
	if cfg.Sudo {
		// sudo resets the environment, so WSLENV alone would drop the token;
		// --preserve-env forwards just that one var without putting it on argv.
		inner += "sudo "
		if cfg.Token != "" {
			inner += "--preserve-env=JERBOA_AUTH_TOKEN "
		}
	}
	inner += shQuote(jerboad) + " --host " + shQuote(listen)
	if cfg.Hypervisor != "" {
		inner += " --hypervisor " + shQuote(cfg.Hypervisor)
	}
	// Forward observability flags in a fixed order (kept stable for testability)
	// so the managed daemon can serve metrics/UI/traces and structured logs.
	for _, kv := range []struct{ flag, val string }{
		{"--metrics-addr", cfg.Observability.MetricsAddr},
		{"--ui-addr", cfg.Observability.UIAddr},
		{"--trace-addr", cfg.Observability.TraceAddr},
		{"--log-format", cfg.Observability.LogFormat},
	} {
		if kv.val != "" {
			inner += " " + kv.flag + " " + shQuote(kv.val)
		}
	}
	// The trailing sleep keeps bash alive just long enough for setsid to enter
	// its new session; when bash exits immediately, WSL's session teardown
	// still kills the freshly forked child (observed empirically).
	inner += " < /dev/null >> " + shQuote(wslLog) + " 2>&1 & sleep 0.5"
	args = append(args, "--", "bash", "-c", inner)

	env = append(env, os.Environ()...)
	if cfg.Token != "" {
		// WSLENV exports the variable into the WSL environment so the daemon
		// reads it without the token ever appearing on a command line.
		env = append(env, "JERBOA_AUTH_TOKEN="+cfg.Token, "WSLENV=JERBOA_AUTH_TOKEN/u")
	}
	return args, env
}

// openLaunchLog opens the daemon launch log (~/.jerboa/jerboad-wsl.log) for the WSL
// process's stdout/stderr. Best-effort: a failure leaves logging disabled.
func openLaunchLog() (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("wslboot: resolve home dir: %w", err)
	}
	dir := filepath.Join(home, ".jerboa")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("wslboot: create log dir: %w", err)
	}
	logPath := filepath.Join(dir, "jerboad-wsl.log")
	rotateIfLarge(logPath, maxLaunchLogBytes)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("wslboot: open launch log: %w", err)
	}
	return f, nil
}

// maxLaunchLogBytes caps the daemon launch log before it is rotated. The log is
// appended to on every (re)start and previously grew without bound and survived
// reinstalls (E2E finding F-011).
const maxLaunchLogBytes = 8 << 20 // 8 MiB

// rotateIfLarge renames path to path+".old" (replacing any previous .old) once it
// exceeds maxBytes, bounding the launch log to at most one prior generation
// instead of unbounded growth. Best-effort: any error just leaves the log as is.
func rotateIfLarge(path string, maxBytes int64) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= maxBytes {
		return
	}
	_ = os.Rename(path, path+".old")
}

// daemonFile is the on-disk shape of ~/.jerboa/daemon.json.
type daemonFile struct {
	Token    string `json:"token"`
	Endpoint string `json:"endpoint,omitempty"`
}

// LoadOrCreateToken returns the token stored at path, generating and persisting
// a new random token (file mode 0600) if the file does not exist yet. The
// client owns this secret and hands it to the daemon at launch.
func LoadOrCreateToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var f daemonFile
		if jerr := json.Unmarshal(data, &f); jerr != nil {
			return "", fmt.Errorf("wslboot: parse %s: %w", path, jerr)
		}
		if f.Token != "" {
			return f.Token, nil
		}
	case !os.IsNotExist(err):
		return "", fmt.Errorf("wslboot: read %s: %w", path, err)
	}

	token, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("wslboot: create dir: %w", err)
	}
	out, err := json.MarshalIndent(daemonFile{Token: token}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("wslboot: marshal token: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", fmt.Errorf("wslboot: write %s: %w", path, err)
	}
	return token, nil
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("wslboot: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
