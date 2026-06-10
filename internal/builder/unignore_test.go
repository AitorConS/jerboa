package builder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewIgnoreMatcher(t *testing.T) {
	m := NewIgnoreMatcher([]string{"*.log", "tmp/"})
	require.True(t, m.Match("debug.log", false))
	require.True(t, m.Match("tmp", true))
	require.False(t, m.Match("main.go", false))
}

func TestLoadIgnoreFileNotFound(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadIgnoreFile(dir)
	require.NoError(t, err)
	require.True(t, m.Match(".git", true))
	require.True(t, m.Match("node_modules", true))
	require.False(t, m.Match("main.go", false))
}

func TestLoadIgnoreFileWithPatterns(t *testing.T) {
	dir := t.TempDir()
	content := `# comment line
*.log
build/
!important.log
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, UnignoreFile), []byte(content), 0o644))

	m, err := LoadIgnoreFile(dir)
	require.NoError(t, err)
	require.True(t, m.Match("debug.log", false))
	require.True(t, m.Match("build", true))
	require.True(t, m.Match(".git", true))
}

func TestDefaultIgnorePatterns(t *testing.T) {
	m := NewIgnoreMatcher(DefaultIgnorePatterns)
	require.True(t, m.Match(".git", true))
	require.True(t, m.Match("node_modules", true))
	require.True(t, m.Match("__pycache__", true))
	require.True(t, m.Match("target", true))
	require.True(t, m.Match("dist", true))
	require.False(t, m.Match("main.go", false))
	require.False(t, m.Match("src/app.go", false))
}

func TestMatchIgnorePatternGlob(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		name    string
		isDir   bool
		want    bool
	}{
		{"*.log", "debug.log", "debug.log", false, true},
		{"*.log", "src/debug.log", "debug.log", false, true},
		{"*.log", "main.go", "main.go", false, false},
		{"build/", "build", "build", true, true},
		{"build/", "build", "build", false, false},
		{".git", ".git", ".git", true, true},
		{"target", "target", "target", true, true},
		{"*.tmp", "src/app.tmp", "app.tmp", false, true},
		{"docs", "docs", "docs", true, true},
		{"docs", "docs/readme.md", "readme.md", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"/"+tt.path, func(t *testing.T) {
			got := matchIgnorePattern(tt.pattern, tt.path, tt.name, tt.isDir)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestMatchIgnorePatternSubpath(t *testing.T) {
	require.True(t, matchIgnorePattern("*.log", "a/b/c/debug.log", "debug.log", false))
	require.True(t, matchIgnorePattern("node_modules", "node_modules/pkg", "pkg", false))
}

func TestMatchIgnorePatternNegation(t *testing.T) {
	require.False(t, matchIgnorePattern("!important.log", "important.log", "important.log", false))
}

func TestMatchNegationOverridesEarlierPattern(t *testing.T) {
	m := NewIgnoreMatcher([]string{"*.log", "!important.log"})
	require.True(t, m.Match("debug.log", false))
	require.False(t, m.Match("important.log", false))
}

func TestMatchNegationOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, UnignoreFile), []byte("!node_modules\n"), 0o644))

	m, err := LoadIgnoreFile(dir)
	require.NoError(t, err)
	require.False(t, m.Match("node_modules", true))
	require.False(t, m.Match("node_modules/pkg/index.js", false))
	require.True(t, m.Match(".git", true))
}

func TestMatchIgnorePatternEmpty(t *testing.T) {
	require.False(t, matchIgnorePattern("", "foo", "foo", false))
	require.False(t, matchIgnorePattern("#", "foo", "foo", false))
}

func TestMatchIgnorePatternDirOnly(t *testing.T) {
	require.True(t, matchIgnorePattern("build/", "build", "build", true))
	require.False(t, matchIgnorePattern("build/", "build", "build", false))
}
