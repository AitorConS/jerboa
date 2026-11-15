package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/AitorConS/jerboa/internal/durable"

	"github.com/pelletier/go-toml/v2"
)

// Config holds global jerboa configuration stored at ~/.jerboa/config.toml.
type Config struct {
	// Hypervisor is "qemu" or "firecracker". Empty leaves the choice to the
	// daemon, which runs Firecracker wherever the host can (see
	// internal/hypervisor).
	Hypervisor string `toml:"hypervisor,omitempty"`
	// Daemon is omitted when empty so writing an unrelated key (e.g.
	// `config set hypervisor`) does not materialize a [daemon] table full of
	// empty endpoint/token/distro strings — which read as "explicitly set to
	// blank" and risked shadowing the real rendezvous in ~/.jerboa/daemon.json
	//.
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
	// than by hand-launching jerboad inside the distro.
	// Persisting them here means both `jerboa daemon start` and the auto-boot path
	// honor them. Empty leaves the daemon's own default.
	MetricsAddr string `toml:"metrics_addr,omitempty"`
	UIAddr      string `toml:"ui_addr,omitempty"`
	TraceAddr   string `toml:"trace_addr,omitempty"`
	LogFormat   string `toml:"log_format,omitempty"`
	// ObservabilityToken protects the managed daemon's metrics and dashboard.
	// It is generated when either is enabled and handed to the daemon through
	// its environment.
	ObservabilityToken string `toml:"observability_token,omitempty"`
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

var (
	sessionTokenMu sync.RWMutex
	sessionToken   string
)

// SetSessionToken records the token this process authenticates with when it
// was found somewhere other than the environment (the config file, or the
// daemon rendezvous file). It stays in the process instead of being exported
// to JERBOA_AUTH_TOKEN, which every child process (build hooks, package
// managers and their install scripts) would inherit.
func SetSessionToken(token string) {
	sessionTokenMu.Lock()
	defer sessionTokenMu.Unlock()
	sessionToken = token
}

// SessionToken returns the token recorded by SetSessionToken.
func SessionToken() string {
	sessionTokenMu.RLock()
	defer sessionTokenMu.RUnlock()
	return sessionToken
}

// ResolveToken returns the client auth token: the JERBOA_AUTH_TOKEN environment
// variable takes precedence over the session token and the config file's
// [daemon] token. Empty means no authentication.
func ResolveToken() string {
	if v := os.Getenv("JERBOA_AUTH_TOKEN"); v != "" {
		return v
	}
	if v := SessionToken(); v != "" {
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
		return filepath.Join(noHomeDir(), "config.toml")
	}
	return filepath.Join(home, ".jerboa", "config.toml")
}

// DaemonFilePath returns the client-owned daemon rendezvous file. The
// fallback mirrors DefaultPath (absolute, under the OS temp dir) so the agent
// and CLI resolve the same file regardless of their working directories.
func DaemonFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(noHomeDir(), "daemon.json")
	}
	return filepath.Join(home, ".jerboa", "daemon.json")
}

// noHomeDir is where the config lives when there is no home directory: a
// per-user directory under the OS temp dir, so one user's files are never
// read by another through a shared path. Load refuses files in it that the
// caller does not own.
func noHomeDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("jerboa-%d", os.Getuid()), ".jerboa")
}

// Load reads the config file at path. Returns defaults if the file does not exist.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	data, err := ReadPrivateFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	// "auto" may be written by hand: it is the same as leaving it out.
	if strings.EqualFold(cfg.Hypervisor, "auto") {
		cfg.Hypervisor = ""
	}
	return cfg, nil
}

// ReadPrivateFile reads path the way Load does: a file in the shared temp
// directory is read only if it is private to this user (see readPrivate), and
// inside the no-home fallback (noHomeDir) the directories leading to it must
// be private too, since another user may have created them first. Other paths
// are read as they are.
func ReadPrivateFile(path string) ([]byte, error) {
	tmp := os.TempDir()
	if !strings.HasPrefix(path, tmp) {
		return os.ReadFile(path)
	}
	path = filepath.Clean(path)
	if inNoHomeDir(path) {
		return readPrivate(tmp, path)
	}
	return readPrivate(filepath.Dir(path), path)
}

// inNoHomeDir reports whether the clean path lies inside noHomeDir.
func inNoHomeDir(path string) bool {
	return strings.HasPrefix(path, noHomeDir()+string(filepath.Separator))
}

// Save writes cfg to path, creating parent directories as needed. The config
// may hold a daemon auth token, so it is kept owner-only.
func Save(path string, cfg *Config) error {
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	return WritePrivateFile(path, data)
}

// WritePrivateFile replaces path with data, owner-only (0600 in a 0700
// directory), creating the directory as needed. Inside the no-home fallback
// each directory below the temp dir is created and checked through its own
// descriptor, and one that is not a real directory of this user is refused,
// so a directory another user created first never receives the file.
func WritePrivateFile(path string, data []byte) error {
	if clean := filepath.Clean(path); strings.HasPrefix(clean, os.TempDir()) && inNoHomeDir(clean) {
		return writePrivate(os.TempDir(), clean, data)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("config: chmod dir: %w", err)
	}
	if err := durable.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("config: chmod %s: %w", path, err)
	}
	return nil
}
