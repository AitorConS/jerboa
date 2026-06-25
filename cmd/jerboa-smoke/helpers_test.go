//go:build linux

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrim(t *testing.T) {
	require.Equal(t, "a b", trim("a", "b"))
	require.Equal(t, "hello", trim("  hello  "))
	require.Empty(t, trim("", "   "))

	long := strings.Repeat("x", 300)
	got := trim(long)
	require.Len(t, got, 223) // 220 chars + "..."
	require.True(t, strings.HasSuffix(got, "..."))
}

func TestHasFail(t *testing.T) {
	require.False(t, hasFail([]result{{name: "a", status: "PASS"}, {name: "b", status: "SKIP"}}))
	require.True(t, hasFail([]result{{name: "a", status: "PASS"}, {name: "b", status: "FAIL"}}))
	require.False(t, hasFail(nil))
}

func TestPrintResults(t *testing.T) {
	// Output goes to stdout; this guards the counting/branching logic against
	// panics and exercises the detail and no-detail paths.
	require.NotPanics(t, func() {
		printResults([]result{
			{name: "ok", status: "PASS"},
			{name: "bad", status: "FAIL", detail: "boom"},
			{name: "skipped", status: "SKIP", detail: "n/a"},
		})
	})
}

func TestStaticClusterListerMembers(t *testing.T) {
	require.NotEmpty(t, staticClusterLister{}.Members())
}
