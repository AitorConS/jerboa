package builder

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const ConfigFileName = "unikernel.toml"

type Config struct {
	Build BuildConfig `toml:"build"`
	Run   RunConfig   `toml:"run"`
	Env   EnvConfig   `toml:"env"`
}

type BuildConfig struct {
	Lang       string   `toml:"lang"`
	Entrypoint string   `toml:"entrypoint"`
	Args       []string `toml:"args"`
}

type RunConfig struct {
	Memory string   `toml:"memory"`
	CPUs   int      `toml:"cpus"`
	Ports  []string `toml:"ports"`
}

type EnvConfig map[string]string

func LoadConfig(dir string) (*Config, error) {
	path := filepath.Join(dir, ConfigFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate config %s: %w", path, err)
	}

	return &cfg, nil
}

func validateConfig(cfg *Config) error {
	if cfg.Build.Lang != "" {
		if _, err := ParseLang(cfg.Build.Lang); err != nil {
			return fmt.Errorf("build.lang: %w", err)
		}
	}

	if cfg.Run.Memory != "" {
		if err := validateMemory(cfg.Run.Memory); err != nil {
			return fmt.Errorf("run.memory: %w", err)
		}
	}

	if cfg.Run.CPUs < 0 {
		return fmt.Errorf("run.cpus: must be non-negative, got %d", cfg.Run.CPUs)
	}

	for i, p := range cfg.Run.Ports {
		if err := validatePortSpec(p); err != nil {
			return fmt.Errorf("run.ports[%d]: %w", i, err)
		}
	}

	return nil
}

func validateMemory(m string) error {
	if m == "" {
		return nil
	}
	s := strings.ToLower(m)
	if !strings.HasSuffix(s, "m") && !strings.HasSuffix(s, "g") && !strings.HasSuffix(s, "mi") && !strings.HasSuffix(s, "gi") {
		return fmt.Errorf("invalid memory format %q: use e.g. 256M or 1G", m)
	}
	numStr := strings.TrimRight(s, "mgi")
	if numStr == "" {
		return fmt.Errorf("invalid memory format %q: missing number", m)
	}
	for _, ch := range numStr {
		if ch < '0' || ch > '9' {
			return fmt.Errorf("invalid memory format %q: non-digit character", m)
		}
	}
	if numStr == "0" {
		return fmt.Errorf("invalid memory format %q: zero value", m)
	}
	return nil
}

func validatePortSpec(p string) error {
	parts := strings.SplitN(p, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid port spec %q: expected host:guest format", p)
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("invalid port spec %q: empty host or guest port", p)
		}
	}
	return nil
}

func (c *Config) LangHint() Lang {
	if c == nil || c.Build.Lang == "" {
		return LangUnknown
	}
	lang, _ := ParseLang(c.Build.Lang)
	return lang
}
