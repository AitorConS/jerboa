package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pelletier/go-toml/v2"
)

// Config holds global jerboa configuration stored at ~/.jerboa/config.toml.
type Config struct {
	Hypervisor string `toml:"hypervisor"`
	// Daemon is omitted when empty so writing an unrelated key (e.g.
	// `config set hypervisor`) does not materialize a [daemon] table full of
	// empty endpoint/token/distro strings — which read as "explicitly set to
	// blank" and risked shadowing the real rendezvous in ~/.jerboa/daemon.json
	// (E2E finding F-002).
	Daemon DaemonConfig `toml:"daemon,omitempty"`
}

// DaemonConfig holds client-side daemon connection settings. Every field is
// omitempty so a partially-populated [daemon] table (e.g. only a token) never
// writes back blank siblings.
type DaemonConfig struct {
	// Endpoint is the daemon address (e.g. unix:///var/run/jerboad.sock or
	// tcp://127.0.0.1:7890). Empty falls back to the per-platform default.
	Endpoint string `toml:"endpoint,omitempty"`
	// Distro is the WSL2 distribution to host the daemon on Windows. Empty uses
	// the WSL default distro.
	Distro string `toml:"distro,omitempty"`
	// JerboadPath is the jerboad binary path inside the WSL distro. Empty resolves
	// "jerboad" on the distro's PATH.
	JerboadPath string `toml:"jerboad_path,omitempty"`
	// Token is the shared secret sent to the daemon via the Auth.Hello
	// handshake. Overridden by the JERBOA_AUTH_TOKEN environment variable.
	Token string `toml:"token,omitempty"`

	// Observability flags below are forwarded to the managed daemon at launch so
	// metrics/UI/tracing/structured logs can be enabled through the CLI rather
	// than by hand-launching jerboad inside the distro (E2E finding F-022).
	// Persisting them here means both `jerboa daemon start` and the auto-boot path
	// honor them. Empty leaves the daemon's own default.
	MetricsAddr string `toml:"metrics_addr,omitempty"`
	UIAddr      string `toml:"ui_addr,omitempty"`
	TraceAddr   string `toml:"trace_addr,omitempty"`
	LogFormat   string `toml:"log_format,omitempty"`
}

// DefaultEndpoint returns the per-platform default daemon endpoint. Windows
// talks to a daemon running inside WSL2 over loopback TCP; other platforms use
// a local Unix socket.
func DefaultEndpoint() string {
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		return "unix://" + filepath.Join(home, ".jerboa", "jerboad.sock")
	}
	if runtime.GOOS == "windows" {
		return "tcp://127.0.0.1:7890"
	}
	return "unix:///var/run/jerboad.sock"
}

// ResolveEndpoint determines the daemon endpoint using the precedence:
// explicit override > JERBOA_HOST env var > config file > platform default.
// override carries the value of an explicit CLI flag (empty if unset).
func ResolveEndpoint(override string) string {
	if override != "" {
		return override
	}
	if v := os.Getenv("JERBOA_HOST"); v != "" {
		return v
	}
	if cfg, err := Load(DefaultPath()); err == nil && cfg.Daemon.Endpoint != "" {
		return cfg.Daemon.Endpoint
	}
	return DefaultEndpoint()
}

// ResolveToken returns the client auth token: the JERBOA_AUTH_TOKEN environment
// variable takes precedence over the config file's [daemon] token. Empty means
// no authentication.
func ResolveToken() string {
	if v := os.Getenv("JERBOA_AUTH_TOKEN"); v != "" {
		return v
	}
	if cfg, err := Load(DefaultPath()); err == nil {
		return cfg.Daemon.Token
	}
	return ""
}

// DefaultPath returns the default config file location (~/.jerboa/config.toml).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".jerboa", "config.toml")
	}
	return filepath.Join(home, ".jerboa", "config.toml")
}

// DaemonFilePath returns the client-owned daemon rendezvous file. The
// fallback mirrors DefaultPath (absolute, under the OS temp dir) so the agent
// and CLI resolve the same file regardless of their working directories.
func DaemonFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".jerboa", "daemon.json")
	}
	return filepath.Join(home, ".jerboa", "daemon.json")
}

// Load reads the config file at path. Returns defaults if the file does not exist.
func Load(path string) (*Config, error) {
	cfg := &Config{Hypervisor: "qemu"}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if cfg.Hypervisor == "" {
		cfg.Hypervisor = "qemu"
	}
	return cfg, nil
}

// Save writes cfg to path, creating parent directories as needed.
func Save(path string, cfg *Config) error {
	// The config may hold a daemon auth token, so keep it owner-only.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("config: chmod dir: %w", err)
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("config: chmod %s: %w", path, err)
	}
	return nil
}
