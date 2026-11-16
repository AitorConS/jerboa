//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lack is one prerequisite this machine does not meet, and how to meet it.
type lack struct {
	what string
	fix  string
}

// preflight lists every prerequisite of the suite this machine lacks. It
// runs before the first test, so that one run on a new runner says
// everything that has to be installed or granted, instead of one failing
// test at a time.
func preflight() []lack {
	var out []lack
	for _, name := range []string{"jerboa", "jerboad"} {
		if _, err := os.Stat(filepath.Join(suite.bin, name)); err != nil {
			out = append(out, lack{name + " is not in " + suite.bin, "make build (or set JERBOA_E2E_BIN)"})
		}
	}
	if _, err := exec.LookPath("go"); err != nil {
		out = append(out, lack{"go is not in PATH (the suite builds its guests with it)", "install Go, or actions/setup-go"})
	}
	return append(out, platformPreflight()...)
}

// reportPreflight prints what preflight found, as annotations in a GitHub
// Actions job, and returns it.
func reportPreflight() []lack {
	lacks := preflight()
	if len(lacks) == 0 {
		fmt.Fprintln(os.Stderr, "e2e preflight: every prerequisite is met")
		return nil
	}
	fmt.Fprintf(os.Stderr, "e2e preflight: %d prerequisite(s) missing on this machine:\n", len(lacks))
	for i, l := range lacks {
		fmt.Fprintf(os.Stderr, "  %d. %s\n     fix: %s\n", i+1, l.what, l.fix)
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			fmt.Fprintf(os.Stderr, "::error title=E2E prerequisite missing::%s. Fix: %s\n", l.what, strings.ReplaceAll(l.fix, "\n", " "))
		}
	}
	return lacks
}

// TestPreflight fails on the CI runner when a prerequisite is missing, with
// the whole list: the tests that need one fail too, each for its own.
func TestPreflight(t *testing.T) {
	if len(suite.lacks) == 0 {
		return
	}
	var b strings.Builder
	for i, l := range suite.lacks {
		fmt.Fprintf(&b, "\n  %d. %s\n     fix: %s", i+1, l.what, l.fix)
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("the runner lacks %d prerequisite(s) of the end-to-end suite:%s", len(suite.lacks), b.String())
	}
	t.Logf("this machine lacks %d prerequisite(s); the tests that need them are skipped:%s", len(suite.lacks), b.String())
}
