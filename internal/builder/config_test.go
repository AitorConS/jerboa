package builder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadConfigNotFound(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(dir)
	require.NoError(t, err)
	require.Nil(t, cfg)
}

func TestLoadConfigValid(t *testing.T) {
	dir := t.TempDir()
	content := `[build]
lang = "go"
entrypoint = "cmd/server"
args = ["-v"]

[run]
memory = "512M"
cpus = 2
ports = ["8080:80", "9090:9090"]

[env]
NODE_ENV = "production"
DEBUG = "false"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	cfg, err := LoadConfig(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, "go", cfg.Build.Lang)
	require.Equal(t, "cmd/server", cfg.Build.Entrypoint)
	require.Equal(t, []string{"-v"}, cfg.Build.Args)
	require.Equal(t, "512M", cfg.Run.Memory)
	require.Equal(t, 2, cfg.Run.CPUs)
	require.Equal(t, []string{"8080:80", "9090:9090"}, cfg.Run.Ports)
	require.Equal(t, "production", cfg.Env["NODE_ENV"])
	require.Equal(t, "false", cfg.Env["DEBUG"])
}

func TestLoadConfigMinimal(t *testing.T) {
	dir := t.TempDir()
	content := `[build]
lang = "python"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	cfg, err := LoadConfig(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, "python", cfg.Build.Lang)
	require.Equal(t, "", cfg.Build.Entrypoint)
	require.Equal(t, 0, cfg.Run.CPUs)
}

func TestLoadConfigInvalidLang(t *testing.T) {
	dir := t.TempDir()
	content := `[build]
lang = "cobol"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	_, err := LoadConfig(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "build.lang")
}

func TestLoadConfigInvalidMemory(t *testing.T) {
	dir := t.TempDir()
	content := `[run]
memory = "abc"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	_, err := LoadConfig(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "run.memory")
}

func TestLoadConfigNegativeCPUs(t *testing.T) {
	dir := t.TempDir()
	content := `[run]
cpus = -1
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	_, err := LoadConfig(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "run.cpus")
}

func TestLoadConfigInvalidPort(t *testing.T) {
	dir := t.TempDir()
	content := `[run]
ports = ["invalid"]
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	_, err := LoadConfig(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "run.ports")
}

func TestLoadConfigInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	content := `this is not valid toml [[[`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	_, err := LoadConfig(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse config")
}

func TestConfigLangHint(t *testing.T) {
	cfg := &Config{Build: BuildConfig{Lang: "go"}}
	require.Equal(t, LangGo, cfg.LangHint())

	cfg2 := &Config{Build: BuildConfig{Lang: ""}}
	require.Equal(t, LangUnknown, cfg2.LangHint())

	require.Equal(t, LangUnknown, (*Config)(nil).LangHint())
}

func TestValidateMemory(t *testing.T) {
	tests := []struct {
		mem string
		ok  bool
	}{
		{"256M", true},
		{"1G", true},
		{"512Mi", true},
		{"2Gi", true},
		{"", true},
		{"abc", false},
		{"M", false},
		{"0M", false},
		{"-1M", false},
		{"128", false},
	}

	for _, tt := range tests {
		err := validateMemory(tt.mem)
		if tt.ok {
			require.NoError(t, err, "expected %q to be valid", tt.mem)
		} else {
			require.Error(t, err, "expected %q to be invalid", tt.mem)
		}
	}
}

func TestValidatePortSpec(t *testing.T) {
	tests := []struct {
		port string
		ok   bool
	}{
		{"8080:80", true},
		{"3000:3000", true},
		{"invalid", false},
		{":80", false},
		{"8080:", false},
	}

	for _, tt := range tests {
		err := validatePortSpec(tt.port)
		if tt.ok {
			require.NoError(t, err, "expected %q to be valid", tt.port)
		} else {
			require.Error(t, err, "expected %q to be invalid", tt.port)
		}
	}
}
