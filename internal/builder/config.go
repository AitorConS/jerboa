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
	Build   BuildConfig   `toml:"build"`
	Run     RunConfig     `toml:"run"`
	Env     EnvConfig     `toml:"env"`
	Program ProgramConfig `toml:"program"`
	Stages  []StageConfig `toml:"stages"`
}

type BuildConfig struct {
	Lang       string   `toml:"lang"`
	Entrypoint string   `toml:"entrypoint"`
	Args       []string `toml:"args"`
	// Run lists shell commands to execute before the language driver packages the project.
	// Equivalent to RUN instructions in a Dockerfile — use for build steps like
	// "npm run build", "nuxt build", "python manage.py collectstatic", etc.
	Run []string `toml:"run"`
	// DiskSize sets the minimum image file size passed to mkfs (e.g. "512M", "1G").
	// Use when the default content-based image size leaves insufficient free space
	// for runtime writes (e.g. database temp tablespaces, log files).
	DiskSize string `toml:"disk_size"`
	// Dirs lists absolute directories to create (empty) inside the image —
	// analogous to a Dockerfile's mkdir/VOLUME. Use for volume mount points (a
	// TFS volume can only be mounted onto a directory that already exists in the
	// root image) and for scratch paths the program writes to at runtime.
	Dirs []string `toml:"dirs"`
}

type RunConfig struct {
	Memory string   `toml:"memory"`
	CPUs   int      `toml:"cpus"`
	Ports  []string `toml:"ports"`
}

// StageConfig defines a build stage in a multi-stage unikernel.toml.
// Each stage can use a different language and copy artifacts from
// a previous stage into the final image.
type StageConfig struct {
	// Name is the stage identifier (required). Referenced by CopyFrom.
	Name string `toml:"name"`
	// Lang is the build language for this stage (e.g. "go", "node").
	Lang string `toml:"lang"`
	// Entrypoint overrides the default entrypoint for the language.
	Entrypoint string `toml:"entrypoint"`
	// Args are extra arguments passed to the build tool.
	Args []string `toml:"args"`
	// CopyFrom lists artifacts to copy from other stages.
	CopyFrom []CopyFromConfig `toml:"copy_from"`
}

// CopyFromConfig describes a file to copy from a previous build stage.
type CopyFromConfig struct {
	// Stage is the name of the source stage.
	Stage string `toml:"stage"`
	// Src is the file path within the source stage's build output.
	Src string `toml:"src"`
	// Dst is the destination path in the current stage (defaults to Src basename).
	Dst string `toml:"dst"`
}

type EnvConfig map[string]string

// ProgramConfig declares the runtime entrypoint for lang = "raw" builds —
// analogous to Docker's ENTRYPOINT (Path) + CMD (Args). Path is resolved
// against the package files supplied via --pkg; Args are passed through
// literally as the program's argv[1..].
type ProgramConfig struct {
	Path string   `toml:"path"`
	Args []string `toml:"args"`
}

func LoadConfig(dir string) (*Config, error) {
	path := filepath.Join(dir, ConfigFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return parseConfig(path, data)
}

// LoadConfigFile loads a config from an explicit file path (the -f/--file flag).
// Unlike LoadConfig, a missing file is an error: the user named a specific file.
func LoadConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return parseConfig(path, data)
}

func parseConfig(path string, data []byte) (*Config, error) {
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

	isRaw := strings.EqualFold(cfg.Build.Lang, "raw")
	switch {
	case isRaw && cfg.Program.Path == "":
		return fmt.Errorf(`program.path is required when build.lang = "raw"`)
	case !isRaw && cfg.Program.Path != "":
		return fmt.Errorf(`program: only valid when build.lang = "raw"`)
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

	if err := validateStages(cfg.Stages); err != nil {
		return err
	}

	for i, d := range cfg.Build.Dirs {
		if err := validateDir(d); err != nil {
			return fmt.Errorf("build.dirs[%d]: %w", i, err)
		}
	}

	return nil
}

// validateDir checks that a dirs entry is a usable absolute, in-image path.
// Paths are slash-style (image paths), must be absolute, and may not escape
// the image root via "..".
func validateDir(d string) error {
	if strings.TrimSpace(d) == "" {
		return fmt.Errorf("empty directory path")
	}
	s := filepath.ToSlash(d)
	if !strings.HasPrefix(s, "/") {
		return fmt.Errorf("must be an absolute path, got %q", d)
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return fmt.Errorf("must not contain %q, got %q", "..", d)
		}
	}
	return nil
}

func validateStages(stages []StageConfig) error {
	seen := make(map[string]bool)
	for i, s := range stages {
		if s.Name == "" {
			return fmt.Errorf("stages[%d]: name is required", i)
		}
		if s.Lang == "" {
			return fmt.Errorf("stages[%d].name=%s: lang is required", i, s.Name)
		}
		if _, err := ParseLang(s.Lang); err != nil {
			return fmt.Errorf("stages[%d].name=%s: %w", i, s.Name, err)
		}
		if seen[s.Name] {
			return fmt.Errorf("stages[%d]: duplicate stage name %q", i, s.Name)
		}
		seen[s.Name] = true

		for j, cf := range s.CopyFrom {
			if cf.Stage == "" {
				return fmt.Errorf("stages[%d].copy_from[%d]: stage is required", i, j)
			}
			if cf.Src == "" {
				return fmt.Errorf("stages[%d].copy_from[%d]: src is required", i, j)
			}
			if cf.Stage == s.Name {
				return fmt.Errorf("stages[%d].copy_from[%d]: cannot copy from self (%q)", i, j, cf.Stage)
			}
		}
	}

	for i, s := range stages {
		for j, cf := range s.CopyFrom {
			if !seen[cf.Stage] {
				return fmt.Errorf("stages[%d].copy_from[%d]: unknown stage %q", i, j, cf.Stage)
			}
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

// HasStages returns true if the config defines multi-stage build stages.
func (c *Config) HasStages() bool {
	return c != nil && len(c.Stages) > 0
}
