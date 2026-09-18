package builder

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIncludeMatcher(t *testing.T) {
	m, err := NewIncludeMatcher([]string{"server.js", "lib", "/public/", "*.json", "**/*.node", "views/**/*.html"})
	require.NoError(t, err)
	require.True(t, m.Active())

	for _, p := range []string{
		"server.js",
		"lib/db.js", "lib/nested/deep/x.js",
		"public/index.html",
		"package.json",
		"node_modules/sharp/build/sharp.node", "addon.node",
		"views/index.html", "views/admin/users/list.html",
	} {
		require.True(t, m.Match(p), p)
	}
	for _, p := range []string{
		"test/server.test.js",
		"libs/other.js",
		"config/app.json", // *.json matches the root only
		"README.md",
		"views/index.txt",
		"server.js.map",
	} {
		require.False(t, m.Match(p), p)
	}
}

func TestIncludeMatcherEmptyIncludesEverything(t *testing.T) {
	m, err := NewIncludeMatcher(nil)
	require.NoError(t, err)
	require.False(t, m.Active())
	require.True(t, m.Match("anything/at/all"))
	var nilMatcher *IncludeMatcher
	require.True(t, nilMatcher.Match("x"))
}

func TestIncludeMatcherRejectsInvalidPatterns(t *testing.T) {
	for _, p := range []string{"", "../secret", "a/./b", "a//b", "[unclosed"} {
		_, err := NewIncludeMatcher([]string{p})
		require.Error(t, err, p)
	}
}
