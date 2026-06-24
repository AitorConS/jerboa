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
	Hypervisor string       `toml:"hypervisor"`
	Daemon     DaemonConfig `toml:"daemon"`
}

// DaemonConfig holds client-side daemon connection settings.
type DaemonConfig struct {
	// Endpoint is the daemon address (e.g. unix:///var/run/jerboad.sock or
	// tcp://127.0.0.1:7890). Empty falls back to the per-platform default.
	Endpoint string `toml:"endpoint"`
	// Distro is the WSL2 distribution to host the daemon on Windows. Empty uses
	// the WSL default distro.
	Distro string `toml:"distro"`
	// JerboadPath is the jerboad binary path inside the WSL distro. Empty resolves
	// "jerboad" on the distro's PATH.
	JerboadPath string `toml:"jerboad_path"`
	// Token is the shared secret sent to the daemon via the Auth.Hello
	// handshake. Overridden by the JERBOA_AUTH_TOKEN environment variable.
	Token string `toml:"token"`
}

// DefaultEndpoint returns the per-platform default daemon endpoint. Windows
// talks to a daemon running inside WSL2 over loopback TCP; other platforms use
// a local Unix socket.
func DefaultEndpoint() string {
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config: create dir: %w", err)
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}
