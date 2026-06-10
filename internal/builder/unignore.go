package builder

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const UnignoreFile = ".unignore"

// DefaultIgnorePatterns are the patterns always excluded from the build context.
var DefaultIgnorePatterns = []string{
	".git",
	".uni-build",
	"node_modules",
	"__pycache__",
	".tox",
	"venv",
	".venv",
	"dist",
	".next",
	"target",
}

// IgnoreMatcher determines whether a file should be excluded from the build context.
type IgnoreMatcher struct {
	patterns []string
}

// NewIgnoreMatcher creates a matcher from the given patterns.
// Patterns follow .gitignore syntax: lines starting with ! negate, lines
// starting with # are comments, trailing / matches directories only.
func NewIgnoreMatcher(patterns []string) *IgnoreMatcher {
	return &IgnoreMatcher{patterns: patterns}
}

// LoadIgnoreFile reads a .unignore file and returns a matcher.
// If the file does not exist, returns a matcher with default patterns only.
func LoadIgnoreFile(dir string) (*IgnoreMatcher, error) {
	path := filepath.Join(dir, UnignoreFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewIgnoreMatcher(DefaultIgnorePatterns), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var patterns []string
	patterns = append(patterns, DefaultIgnorePatterns...)

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	return NewIgnoreMatcher(patterns), nil
}

// Match returns true if relPath should be excluded from the build context.
// relPath should be a forward-slash-separated path relative to the project root.
// Patterns are evaluated in order; a later "!pattern" re-includes a path
// excluded by an earlier pattern (gitignore-style negation).
func (m *IgnoreMatcher) Match(relPath string, isDir bool) bool {
	name := filepath.Base(relPath)
	excluded := false
	for _, pattern := range m.patterns {
		negate := strings.HasPrefix(pattern, "!")
		p := strings.TrimPrefix(pattern, "!")
		if matchIgnorePattern(p, relPath, name, isDir) {
			excluded = !negate
		}
	}
	return excluded
}

// matchIgnorePattern matches a single .gitignore-style pattern against a path.
func matchIgnorePattern(pattern, relPath, name string, isDir bool) bool {
	if strings.HasPrefix(pattern, "!") {
		return false
	}

	dirOnly := strings.HasSuffix(pattern, "/")
	cleanPattern := strings.TrimSuffix(pattern, "/")
	cleanPattern = strings.TrimPrefix(cleanPattern, "/")

	if cleanPattern == "" {
		return false
	}

	if dirOnly && !isDir {
		return false
	}

	matched, _ := filepath.Match(cleanPattern, name)
	if matched {
		return true
	}

	matched, _ = filepath.Match(cleanPattern, relPath)
	if matched {
		return true
	}

	for _, part := range strings.Split(relPath, "/") {
		matched, _ = filepath.Match(cleanPattern, part)
		if matched {
			return true
		}
	}

	return false
}
