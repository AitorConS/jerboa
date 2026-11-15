package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/spf13/cobra"
)

// smokeWindow is how long the smoke test observes the freshly booted VM before
// declaring it healthy. Long enough for Nanos to boot and the program to hit
// its first failure (missing lib, fork attempt, OOM), short enough to keep
// `build --smoke` interactive.
const smokeWindow = 8 * time.Second

// runSmokeTest boots the just-built image once, watches its serial output for
// known Nanos failure signatures, and tears the test VM down again. It returns
// an error when the guest hit a recognizable failure or produced no output at
// all before exiting — both signs the image would not serve traffic.
//
// A VM that stops within the window without a failure signature is treated as
// success as long as it printed something: short-lived batch programs (e.g.
// examples/hello) legitimately boot, print, and exit.
func runSmokeTest(cmd *cobra.Command, client *api.Client, ref string) error {
	ctx := cmd.Context()
	errW := cmd.ErrOrStderr()

	fmt.Fprintf(errW, "Smoke test: booting %s ...\n", ref)
	info, err := client.Run(ctx, api.RunParams{Image: ref})
	if err != nil {
		return fmt.Errorf("build: smoke test: boot failed before the VM started: %w", err)
	}
	id := info.ID

	// Always tear the test VM down, even on failure paths: stop is best-effort
	// (the VM may already have exited), remove cleans the record. A server image
	// (e.g. webenv) is still "running" at teardown, so a force-stop returns while
	// the VM is transitioning through "stopping"; removeWhenStopped retries until
	// it settles so no orphan VM is left behind (E2E finding).
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx, id, true)
		_ = removeWhenStopped(stopCtx, client, id)
	}()

	deadline := time.After(smokeWindow)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()

	var logs string
	state := info.State
	for done := false; !done; {
		select {
		case <-ctx.Done():
			return fmt.Errorf("smoke test interrupted: %w", ctx.Err())
		case <-deadline:
			done = true
		case <-tick.C:
			if resp, lerr := client.Logs(ctx, id); lerr == nil {
				logs = resp.Logs
			}
			if vi, gerr := client.Get(ctx, id); gerr == nil {
				state = vi.State
				if state == "stopped" {
					done = true
				}
			}
			// Fail fast on a recognized failure signature instead of waiting out
			// the whole window.
			if len(scanNanosHints(logs)) > 0 {
				done = true
			}
		}
	}

	if hints := scanNanosHints(logs); len(hints) > 0 {
		fmt.Fprintf(errW, "Smoke test failed — guest output:\n%s\n", indentLines(tailLines(sanitizeTerminal(logs), 20)))
		printNanosHints(errW, logs)
		return fmt.Errorf("build: smoke test: the image booted but the program failed (see hints above)")
	}
	if state == "stopped" && strings.TrimSpace(logs) == "" {
		return fmt.Errorf("build: smoke test: the VM exited without producing any output — the image likely failed before the program ran (try 'jerboa run %s --attach')", ref)
	}

	fmt.Fprintf(errW, "Smoke test passed: %s booted and ran without known failure signatures (state: %s)\n", ref, state)
	return nil
}

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// indentLines prefixes every line of s with two spaces.
func indentLines(s string) string {
	if s == "" {
		return s
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
