package builder

import (
	"fmt"
	"path"
	"strings"
)

// IncludeMatcher implements the [build] include allowlist: when patterns are
// set, only build-context files matching at least one of them enter the image.
// It narrows what .unignore already lets through; it never re-includes a path
// that an ignore rule (including the defaults such as node_modules) excludes.
//
// Patterns are slash-separated and relative to the project root:
//   - "*", "?" and "[...]" match within one path segment (path.Match syntax);
//   - "**" matches any number of segments, including none;
//   - a pattern that names a directory also matches everything below it.
//
// So "server.js" matches one file, "lib" matches lib/ recursively, "*.js"
// matches JavaScript files at the root only and "**/*.js" matches them anywhere.
type IncludeMatcher struct {
	patterns [][]string
}

// NewIncludeMatcher validates patterns. An empty list includes everything.
func NewIncludeMatcher(patterns []string) (*IncludeMatcher, error) {
	m := &IncludeMatcher{}
	for _, p := range patterns {
		clean := strings.Trim(strings.TrimSpace(p), "/")
		if clean == "" {
			return nil, fmt.Errorf("include: empty pattern")
		}
		segs := strings.Split(clean, "/")
		for _, s := range segs {
			if s == ".." || s == "." || s == "" {
				return nil, fmt.Errorf("include: pattern %q must be a clean path relative to the project root", p)
			}
			if s == "**" {
				continue
			}
			if _, err := path.Match(s, ""); err != nil {
				return nil, fmt.Errorf("include: pattern %q: %w", p, err)
			}
		}
		m.patterns = append(m.patterns, segs)
	}
	return m, nil
}

// Active reports whether the allowlist restricts anything.
func (m *IncludeMatcher) Active() bool { return m != nil && len(m.patterns) > 0 }

// Match reports whether the slash-separated, root-relative file path relPath
// is allowed.
func (m *IncludeMatcher) Match(relPath string) bool {
	if !m.Active() {
		return true
	}
	segs := strings.Split(strings.Trim(relPath, "/"), "/")
	for _, p := range m.patterns {
		// A match on a leading part of the path means the pattern named one of
		// the file's parent directories.
		for n := 1; n <= len(segs); n++ {
			if matchSegments(p, segs[:n]) {
				return true
			}
		}
	}
	return false
}

// matchSegments matches pattern segments against path segments with "**"
// spanning zero or more segments.
func matchSegments(pattern, segs []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pattern[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pattern[0], segs[0]); !ok {
			return false
		}
		pattern, segs = pattern[1:], segs[1:]
	}
	return len(segs) == 0
}
