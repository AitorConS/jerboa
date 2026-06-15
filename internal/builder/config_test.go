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

func TestLoadConfigBuildRun(t *testing.T) {
	dir := t.TempDir()
	content := `[build]
lang = "node"
entrypoint = ".output/server/index.mjs"
run = ["npm install", "npm run build"]
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	cfg, err := LoadConfig(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, []string{"npm install", "npm run build"}, cfg.Build.Run)
	require.Equal(t, ".output/server/index.mjs", cfg.Build.Entrypoint)
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

func TestLoadConfigRawProgram(t *testing.T) {
	dir := t.TempDir()
	content := `[build]
lang = "raw"
run = ["mvn -q -DskipTests package"]

[program]
path = "java"
args = ["-jar", "/app.jar"]
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	cfg, err := LoadConfig(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, "raw", cfg.Build.Lang)
	require.Equal(t, LangRaw, cfg.LangHint())
	require.Equal(t, "java", cfg.Program.Path)
	require.Equal(t, []string{"-jar", "/app.jar"}, cfg.Program.Args)
}

func TestLoadConfigRawWithoutProgram(t *testing.T) {
	dir := t.TempDir()
	content := `[build]
lang = "raw"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	_, err := LoadConfig(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), `program.path is required when build.lang = "raw"`)
}

func TestLoadConfigProgramWithoutRaw(t *testing.T) {
	dir := t.TempDir()
	content := `[build]
lang = "go"

[program]
path = "java"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	_, err := LoadConfig(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), `program: only valid when build.lang = "raw"`)
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

func TestLoadConfigWithStages(t *testing.T) {
	dir := t.TempDir()
	content := `[build]
lang = "go"

[run]
memory = "512M"

[[stages]]
name = "builder"
lang = "go"
entrypoint = "cmd/server"

[[stages]]
name = "runtime"
lang = "node"
entrypoint = "server.js"

[[stages.copy_from]]
stage = "builder"
src = "/app/server"
dst = "server"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(content), 0o644))

	cfg, err := LoadConfig(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Len(t, cfg.Stages, 2)
	require.Equal(t, "builder", cfg.Stages[0].Name)
	require.Equal(t, "go", cfg.Stages[0].Lang)
	require.Equal(t, "runtime", cfg.Stages[1].Name)
	require.Equal(t, "node", cfg.Stages[1].Lang)
	require.Len(t, cfg.Stages[1].CopyFrom, 1)
	require.Equal(t, "builder", cfg.Stages[1].CopyFrom[0].Stage)
	require.Equal(t, "/app/server", cfg.Stages[1].CopyFrom[0].Src)
	require.Equal(t, "server", cfg.Stages[1].CopyFrom[0].Dst)
}

func TestValidateStages(t *testing.T) {
	tests := []struct {
		name    string
		stages  []StageConfig
		wantErr string
	}{
		{
			name: "valid stages",
			stages: []StageConfig{
				{Name: "builder", Lang: "go"},
				{Name: "runtime", Lang: "node", CopyFrom: []CopyFromConfig{
					{Stage: "builder", Src: "/app/server"},
				}},
			},
			wantErr: "",
		},
		{
			name:    "missing name",
			stages:  []StageConfig{{Name: "", Lang: "go"}},
			wantErr: "name is required",
		},
		{
			name:    "missing lang",
			stages:  []StageConfig{{Name: "builder", Lang: ""}},
			wantErr: "lang is required",
		},
		{
			name:    "invalid lang",
			stages:  []StageConfig{{Name: "builder", Lang: "cobol"}},
			wantErr: "unsupported language",
		},
		{
			name: "duplicate name",
			stages: []StageConfig{
				{Name: "builder", Lang: "go"},
				{Name: "builder", Lang: "node"},
			},
			wantErr: "duplicate stage name",
		},
		{
			name: "copy from self",
			stages: []StageConfig{
				{Name: "builder", Lang: "go", CopyFrom: []CopyFromConfig{
					{Stage: "builder", Src: "/app"},
				}},
			},
			wantErr: "cannot copy from self",
		},
		{
			name: "copy from unknown",
			stages: []StageConfig{
				{Name: "runtime", Lang: "node", CopyFrom: []CopyFromConfig{
					{Stage: "nonexistent", Src: "/app"},
				}},
			},
			wantErr: "unknown stage",
		},
		{
			name: "copy from missing src",
			stages: []StageConfig{
				{Name: "builder", Lang: "go"},
				{Name: "runtime", Lang: "node", CopyFrom: []CopyFromConfig{
					{Stage: "builder", Src: ""},
				}},
			},
			wantErr: "src is required",
		},
		{
			name: "copy from missing stage ref",
			stages: []StageConfig{
				{Name: "builder", Lang: "go"},
				{Name: "runtime", Lang: "node", CopyFrom: []CopyFromConfig{
					{Stage: "", Src: "/app"},
				}},
			},
			wantErr: "stage is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStages(tt.stages)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestConfigHasStages(t *testing.T) {
	cfg := &Config{Stages: []StageConfig{{Name: "builder", Lang: "go"}}}
	require.True(t, cfg.HasStages())

	cfg2 := &Config{}
	require.False(t, cfg2.HasStages())

	require.False(t, (*Config)(nil).HasStages())
}
