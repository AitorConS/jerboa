package builder

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseLang(t *testing.T) {
	tests := []struct {
		input   string
		want    Lang
		wantErr bool
	}{
		{"go", LangGo, false},
		{"Go", LangGo, false},
		{"GO", LangGo, false},
		{"node", LangNode, false},
		{"nodejs", LangNode, false},
		{"NodeJS", LangNode, false},
		{"python", LangPython, false},
		{"py", LangPython, false},
		{"rust", LangRust, false},
		{"Rust", LangRust, false},
		{"raw", LangRaw, false},
		{"RAW", LangRaw, false},
		{"unknown", LangUnknown, true},
		{"", LangUnknown, true},
		{"java", LangUnknown, true},
	}
	for _, tt := range tests {
		got, err := ParseLang(tt.input)
		if tt.wantErr {
			require.Error(t, err)
			require.Equal(t, LangUnknown, got)
		} else {
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		}
	}
}

func TestLangString(t *testing.T) {
	tests := []struct {
		lang Lang
		want string
	}{
		{LangGo, "go"},
		{LangNode, "node"},
		{LangPython, "python"},
		{LangRust, "rust"},
		{LangRaw, "raw"},
		{LangUnknown, "unknown"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, tt.lang.String())
	}
}

func TestDetectLanguageNoMarkers(t *testing.T) {
	dir := t.TempDir()
	lang, err := DetectLanguage(dir, LangUnknown)
	require.Error(t, err)
	require.Equal(t, LangUnknown, lang)
}

func TestDetectLanguagePackageJSON(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))
	lang, err := DetectLanguage(dir, LangUnknown)
	require.NoError(t, err)
	require.Equal(t, LangNode, lang)
}

func TestDetectLanguagePyprojectToml(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0o644))
	lang, err := DetectLanguage(dir, LangUnknown)
	require.NoError(t, err)
	require.Equal(t, LangPython, lang)
}

func TestDetectLanguageRequirementsTxt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0o644))
	lang, err := DetectLanguage(dir, LangUnknown)
	require.NoError(t, err)
	require.Equal(t, LangPython, lang)
}

func TestDetectLanguageCargoToml(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"app\"\n"), 0o644))
	lang, err := DetectLanguage(dir, LangUnknown)
	require.NoError(t, err)
	require.Equal(t, LangRust, lang)
}

func TestDetectLanguageWithHint(t *testing.T) {
	dir := t.TempDir()
	lang, err := DetectLanguage(dir, LangGo)
	require.NoError(t, err)
	require.Equal(t, LangGo, lang)
}

func TestDetectLanguageGoMod(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/myapp\ngo 1.22\n"), 0o644))
	lang, err := DetectLanguage(dir, LangUnknown)
	require.NoError(t, err)
	require.Equal(t, LangGo, lang)
}

func TestDetectLanguageAmbiguous(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	lang, err := DetectLanguage(dir, LangUnknown)
	require.Error(t, err)
	require.Equal(t, LangUnknown, lang)
	require.Contains(t, err.Error(), "ambiguous")

	lang2, err2 := DetectLanguage(dir, LangGo)
	require.NoError(t, err2)
	require.Equal(t, LangGo, lang2)
}

func TestAvailableDriversIncludesAll(t *testing.T) {
	drivers := AvailableDrivers()
	langs := make(map[Lang]bool)
	for _, d := range drivers {
		langs[d.Lang()] = true
	}
	require.True(t, langs[LangGo])
	require.True(t, langs[LangNode])
	require.True(t, langs[LangPython])
	require.True(t, langs[LangRust])
	require.True(t, langs[LangRaw])
}

func TestRawDriver(t *testing.T) {
	dir := t.TempDir()
	d := &RawDriver{}

	require.Equal(t, LangRaw, d.Lang())
	require.False(t, d.Detect(dir))

	result, err := d.Build(context.Background(), dir, Options{})
	require.NoError(t, err)
	require.Equal(t, dir, result.SourceDir)
	require.Empty(t, result.BinaryPath)
	require.Empty(t, result.Packages)
}

func TestNodeDriverDetect(t *testing.T) {
	dir := t.TempDir()
	d := &NodeDriver{}
	require.False(t, d.Detect(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))
	require.True(t, d.Detect(dir))
}

func TestNodeEntrypoint(t *testing.T) {
	tests := []struct {
		name     string
		override string
		pkgJSON  string
		want     string
	}{
		{
			name:     "default when no main",
			pkgJSON:  `{"name": "app"}`,
			override: "",
			want:     "index.js",
		},
		{
			name:     "main field from package.json",
			pkgJSON:  `{"name": "app", "main": "server.js"}`,
			override: "",
			want:     "server.js",
		},
		{
			name:     "override takes priority",
			pkgJSON:  `{"name": "app", "main": "server.js"}`,
			override: "app.js",
			want:     "app.js",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(tt.pkgJSON), 0o644))
			if tt.override != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, tt.override), []byte(""), 0o644))
			}

			got, err := nodeEntrypoint(dir, tt.override)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestNodeVersionFromPackageJSON(t *testing.T) {
	tests := []struct {
		name    string
		pkgJSON string
		want    string
	}{
		{"no engines", `{"name": "app"}`, "20"},
		{"exact version", `{"engines": {"node": "18.0.0"}}`, "18"},
		{"caret version", `{"engines": {"node": "^20.10.0"}}`, "20"},
		{"tilde version", `{"engines": {"node": "~16.14.0"}}`, "16"},
		{"gte version", `{"engines": {"node": ">=18.0.0"}}`, "18"},
		{"star version", `{"engines": {"node": "*"}}`, "20"},
		{"empty version", `{"engines": {"node": ""}}`, "20"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(tt.pkgJSON), 0o644))
			got, err := nodeVersionFromPackageJSON(dir)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestNodeVersionFromPackageJSONNoFile(t *testing.T) {
	dir := t.TempDir()
	got, err := nodeVersionFromPackageJSON(dir)
	require.NoError(t, err)
	require.Equal(t, "20", got)
}

func TestPythonDriverDetect(t *testing.T) {
	dir := t.TempDir()
	d := &PythonDriver{}
	require.False(t, d.Detect(dir))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0o644))
	require.True(t, d.Detect(dir))

	dir2 := t.TempDir()
	require.False(t, d.Detect(dir2))
	require.NoError(t, os.WriteFile(filepath.Join(dir2, "requirements.txt"), []byte("flask\n"), 0o644))
	require.True(t, d.Detect(dir2))
}

func TestPythonEntrypoint(t *testing.T) {
	tests := []struct {
		name      string
		override  string
		pyproject string
		want      string
	}{
		{
			name:      "default",
			pyproject: "",
			override:  "",
			want:      "main.py",
		},
		{
			name:      "override takes priority",
			override:  "app.py",
			pyproject: "[project]\nscripts = {start = \"server.py\"}\n",
			want:      "app.py",
		},
		{
			name:      "from pyproject scripts",
			pyproject: "[project]\nscripts = {start = \"server.py\"}\n",
			override:  "",
			want:      "server.py",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.pyproject != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(tt.pyproject), 0o644))
			}
			if tt.override != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, tt.override), []byte(""), 0o644))
			}

			got, err := pythonEntrypoint(dir, tt.override)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestPythonVersionFromConfig(t *testing.T) {
	tests := []struct {
		name      string
		pyproject string
		want      string
	}{
		{"no file", "", "3.12"},
		{"no requires-python", "[project]\nname = \"app\"\n", "3.12"},
		{"exact version", "[project]\nrequires-python = \">=3.11.0\"\n", "3.11"},
		{"caret version", "[project]\nrequires-python = \"^3.10\"\n", "3.10"},
		{"tilde version", "[project]\nrequires-python = \"~3.9\"\n", "3.9"},
		{"gte", "[project]\nrequires-python = \">=3.12\"\n", "3.12"},
		{"major only", "[project]\nrequires-python = \">=3\"\n", "3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.pyproject != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(tt.pyproject), 0o644))
			}
			got, err := pythonVersionFromConfig(dir)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestPythonVersionFromConfigNoFile(t *testing.T) {
	dir := t.TempDir()
	got, err := pythonVersionFromConfig(dir)
	require.NoError(t, err)
	require.Equal(t, "3.12", got)
}

func TestRustDriverDetect(t *testing.T) {
	dir := t.TempDir()
	d := &RustDriver{}
	require.False(t, d.Detect(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"app\"\n"), 0o644))
	require.True(t, d.Detect(dir))
}

func TestRustBinaryName(t *testing.T) {
	dir := t.TempDir()
	content := `[package]
name = "myserver"
version = "0.1.0"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(content), 0o644))
	name, err := rustBinaryName(dir)
	require.NoError(t, err)
	require.Equal(t, "myserver", name)
}

func TestRustBinaryNameMissing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\n"), 0o644))
	_, err := rustBinaryName(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing package.name")
}

func TestGetDriverGo(t *testing.T) {
	d, err := GetDriver(LangGo)
	require.NoError(t, err)
	require.Equal(t, LangGo, d.Lang())
}

func TestGetDriverUnknown(t *testing.T) {
	_, err := GetDriver(LangUnknown)
	require.Error(t, err)
}

func TestGetDriverRaw(t *testing.T) {
	d, err := GetDriver(LangRaw)
	require.NoError(t, err)
	require.Equal(t, LangRaw, d.Lang())
}

func TestGoDriverDetect(t *testing.T) {
	dir := t.TempDir()
	d := &GoDriver{}

	require.False(t, d.Detect(dir), "empty dir should not detect Go")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/myapp\n"), 0o644))
	require.True(t, d.Detect(dir), "dir with go.mod should detect Go")
}

func TestGoDriverBuild(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Go driver build test requires a Unix-like system with CGO cross-compile support")
	}

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/testapp\ngo 1.22\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hello\") }\n"), 0o644))

	d := &GoDriver{}
	result, err := d.Build(context.Background(), dir, Options{})
	require.NoError(t, err)
	require.NotEmpty(t, result.BinaryPath)

	_, err = os.Stat(result.BinaryPath)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = os.Remove(result.BinaryPath)
	})
}

func TestGoDriverBuildWithEntrypoint(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Go driver build test requires a Unix-like system")
	}

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/testapp\ngo 1.22\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cmd", "server"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd", "server", "main.go"), []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"server\") }\n"), 0o644))

	d := &GoDriver{}
	result, err := d.Build(context.Background(), dir, Options{Entrypoint: "./cmd/server"})
	require.NoError(t, err)
	require.NotEmpty(t, result.BinaryPath)

	t.Cleanup(func() {
		_ = os.Remove(result.BinaryPath)
	})
}
