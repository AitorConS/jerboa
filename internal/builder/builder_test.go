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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/myapp\n"), 0o644))
	// Ambiguity only occurs when multiple drivers detect markers.
	// With only GoDriver registered, a go.mod alone is unambiguous.
	// Add a fake marker that another driver would detect.
	// For now, verify that a single detected language returns cleanly.
	lang, err := DetectLanguage(dir, LangUnknown)
	require.NoError(t, err)
	require.Equal(t, LangGo, lang)

	// Test ambiguity by adding package.json and verifying that
	// DetectLanguage returns an error when no hint is given AND
	// multiple markers exist. Since only GoDriver is registered,
	// we test the hint-override path instead.
	ndir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ndir, "go.mod"), []byte("module x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ndir, "package.json"), []byte("{}"), 0o644))
	// Only Go driver registered, so only Go detected (not ambiguous yet).
	lang2, err2 := DetectLanguage(ndir, LangGo)
	require.NoError(t, err2)
	require.Equal(t, LangGo, lang2)
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
