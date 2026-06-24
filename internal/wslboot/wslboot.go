// Package wslboot bootstraps the unid daemon inside WSL2 from the Windows
// client. The daemon always runs on Linux; on Windows uni.exe is a thin client
// that health-checks a loopback TCP endpoint and, if nothing answers, launches
// unid inside a WSL2 distribution and waits for it to come up.
package wslboot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
)

// Config describes how to reach and, if needed, launch the daemon.
type Config struct {
	// Endpoint is the daemon address, e.g. tcp://127.0.0.1:7890.
	Endpoint string
	// Distro is the WSL2 distribution name. Empty uses the WSL default distro.
	Distro string
	// Token is the shared secret passed to the daemon (via the environment, not
	// argv) and used by the client handshake. Empty disables authentication.
	Token string
	// UnidPath is the unid binary path inside WSL. Empty resolves "unid" on the
	// distro's PATH.
	UnidPath string
	// HealthTimeout bounds how long to wait for a freshly launched daemon to
	// answer. Zero defaults to 20s.
	HealthTimeout time.Duration
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
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// launchInWSL starts unid inside the configured WSL distro. The launching
// `wsl` process is detached and not waited on: it runs unid in the foreground
// inside WSL, which keeps the daemon alive after the client exits. (Background
// jobs started with `&` inside a one-shot `wsl -- ...` invocation are reaped
// when that invocation returns, so foreground + detach is required.) The auth
// token is supplied through the environment, never argv.
func launchInWSL(cfg Config) error {
	args, env := buildLaunchArgs(cfg)
	cmd := exec.Command("wsl", args...) //nolint:gosec,noctx // fixed program, controlled args; must outlive caller
	cmd.Env = env
	cmd.SysProcAttr = detachAttr()
	if logf, err := openLaunchLog(); err == nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("wsl launch: %w", err)
	}
	// Do not Wait: the process must outlive this client invocation. Releasing
	// the handle lets the OS reap it independently.
	_ = cmd.Process.Release()
	return nil
}

// buildLaunchArgs builds the `wsl` arguments and process environment that start
// unid in the foreground inside the distro. Exposed (unexported) for testing.
func buildLaunchArgs(cfg Config) (args, env []string) {
	unid := cfg.UnidPath
	if unid == "" {
		unid = "unid"
	}
	if cfg.Distro != "" {
		args = append(args, "-d", cfg.Distro)
	}
	args = append(args, "--", unid, "--host", cfg.Endpoint)

	env = append(env, os.Environ()...)
	if cfg.Token != "" {
		// WSLENV exports the variable into the WSL environment so the daemon
		// reads it without the token ever appearing on a command line.
		env = append(env, "UNI_AUTH_TOKEN="+cfg.Token, "WSLENV=UNI_AUTH_TOKEN/u")
	}
	return args, env
}

// openLaunchLog opens the daemon launch log (~/.uni/unid-wsl.log) for the WSL
// process's stdout/stderr. Best-effort: a failure leaves logging disabled.
func openLaunchLog() (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".uni")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, "unid-wsl.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// daemonFile is the on-disk shape of ~/.uni/daemon.json.
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
