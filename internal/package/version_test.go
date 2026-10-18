package pkg

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"20.11.0", "20.9.0", 1},
		{"20.11.0", "20.11.0", 0},
		{"v20.11.0", "20.11.0", 0},
		{"22.0.0", "20.11.0", 1},
		{"20.11.1", "20.11", 1},
		{"1.0-rc1", "1.0", -1},
		{"1.0-rc2", "1.0-rc1", 1},
		{"3.12.0", "3.9.18", 1},
		{"1.37", "1.36.1", 1},
		{"8.0.2", "7.2.4", 1},
	}
	for _, c := range cases {
		got := CompareVersions(c.a, c.b)
		switch {
		case c.want > 0:
			require.Positive(t, got, "%s vs %s", c.a, c.b)
		case c.want < 0:
			require.Negative(t, got, "%s vs %s", c.a, c.b)
		default:
			require.Zero(t, got, "%s vs %s", c.a, c.b)
		}
	}
}

func TestVersionMatch(t *testing.T) {
	require.Equal(t, 2, VersionMatch("20.11.0", "20.11.0"))
	require.Equal(t, 2, VersionMatch("v20.11.0", "20.11.0"))
	require.Equal(t, 1, VersionMatch("20", "20.11.0"))
	require.Equal(t, 1, VersionMatch("20", "v20.11.0"))
	require.Equal(t, 1, VersionMatch("20.11", "20.11.0"))
	require.Equal(t, 0, VersionMatch("20", "200.1.0"))
	require.Equal(t, 0, VersionMatch("20.1", "20.11.0"))
	require.Equal(t, 1, VersionMatch("latest", "1.0"))
	require.Equal(t, 1, VersionMatch("", "1.0"))
}

func TestRankByVersionPrefersExactThenHighest(t *testing.T) {
	pkgs := []Package{
		{Name: "node", Version: "20.9.0"},
		{Name: "node", Version: "22.0.0"},
		{Name: "node", Version: "20.11.0"},
		{Name: "node", Version: "20"},
	}
	got := rankByVersion(pkgs, "20", nil)
	var versions []string
	for _, p := range got {
		versions = append(versions, p.Version)
	}
	require.Equal(t, []string{"20", "20.11.0", "20.9.0"}, versions)

	got = rankByVersion(pkgs, "latest", nil)
	require.Equal(t, "22.0.0", got[0].Version)
}

func TestIndexLatestIgnoresPublicationOrder(t *testing.T) {
	// package-index.py appends new versions at the end of the list.
	idx := &Index{Packages: map[string][]Package{"node": {
		{Name: "node", Version: "20.11.0"},
		{Name: "node", Version: "22.0.0"},
	}}}
	require.Equal(t, "22.0.0", idx.Latest("node").Version)
}
