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
}

func TestNodeDriverDetect(t *testing.T) {
	dir := t.TempDir()
	d := &NodeDriver{}
	require.False(t, d.Detect(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))
	require.True(t, d.Detect(dir))
}

func TestNodeDriverBuildNotImplemented(t *testing.T) {
	d := &NodeDriver{}
	_, err := d.Build(context.Background(), "", Options{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not yet implemented")
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

func TestPythonDriverBuildNotImplemented(t *testing.T) {
	d := &PythonDriver{}
	_, err := d.Build(context.Background(), "", Options{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not yet implemented")
}

func TestRustDriverDetect(t *testing.T) {
	dir := t.TempDir()
	d := &RustDriver{}
	require.False(t, d.Detect(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"app\"\n"), 0o644))
	require.True(t, d.Detect(dir))
}

func TestRustDriverBuildNotImplemented(t *testing.T) {
	d := &RustDriver{}
	_, err := d.Build(context.Background(), "", Options{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not yet implemented")
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
