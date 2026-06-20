package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// All spinner tests run in non-TTY mode (bytes.Buffer is not a terminal).

func TestSpinner_NonTTY_Start_Done(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, false)

	sp.Start("Loading data")
	sp.Done("Loaded successfully")

	out := buf.String()
	require.Contains(t, out, "Loading data...")
	require.Contains(t, out, "✓ Loaded successfully")
}

func TestSpinner_NonTTY_Start_Fail(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, false)

	sp.Start("Building")
	sp.Fail("Build failed")

	out := buf.String()
	require.Contains(t, out, "Building...")
	require.Contains(t, out, "✗ Build failed")
}

func TestSpinner_Fail_FlushesSubprocessOutput(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, false)

	sp.Start("Building")
	w := sp.SubWriter()
	_, _ = w.Write([]byte("error: missing dependency\n"))
	sp.Fail("Build failed")

	out := buf.String()
	require.Contains(t, out, "✗ Build failed")
	require.Contains(t, out, "error: missing dependency")
}

func TestSpinner_Done_DoesNotFlushSubprocessOutput(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, false)

	sp.Start("Building")
	_, _ = sp.SubWriter().Write([]byte("some noisy output"))
	sp.Done("Build complete")

	out := buf.String()
	require.Contains(t, out, "✓ Build complete")
	require.NotContains(t, out, "some noisy output", "subprocess output must be suppressed on success")
}

func TestSpinner_Update_NonTTY(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, false)

	sp.Start("Phase 1")
	sp.Update("Phase 2")
	sp.Done("Done")

	out := buf.String()
	require.Contains(t, out, "Phase 1...")
	require.Contains(t, out, "Phase 2...")
}

func TestSpinner_MultipleSteps(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, false)

	sp.Start("Step A")
	sp.Done("A complete")
	sp.Start("Step B")
	sp.Done("B complete")

	out := buf.String()
	require.Contains(t, out, "✓ A complete")
	require.Contains(t, out, "✓ B complete")
}

func TestSpinner_Verbose_StartAndDone_PrintNothing(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, true)

	sp.Start("Loading")
	sp.Done("Loaded")

	// In verbose mode, Start and Done are silent — raw subprocess output flows through instead.
	require.Empty(t, buf.String())
}

func TestSpinner_Verbose_Fail_PrintsMarker(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, true)

	sp.Start("Building")
	sp.Fail("Build failed")

	// Fail always prints the marker, even in verbose mode.
	require.Contains(t, buf.String(), "✗ Build failed")
}

func TestSpinner_Verbose_SubWriter_PassesThrough(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, true)

	_, _ = sp.SubWriter().Write([]byte("pip install flask\nCollecting flask...\n"))

	// In verbose mode SubWriter writes directly to the main writer.
	out := buf.String()
	require.Contains(t, out, "pip install flask")
	require.Contains(t, out, "Collecting flask...")
}

func TestSpinner_Fail_ClearsBufferAfterFlush(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, false)

	sp.Start("Step 1")
	_, _ = sp.SubWriter().Write([]byte("output from step 1"))
	sp.Fail("Step 1 failed")
	buf.Reset()

	// Start a new step — the old buffered output must not bleed through.
	sp.Start("Step 2")
	sp.Done("Step 2 done")

	require.NotContains(t, buf.String(), "output from step 1")
}

func TestSpinner_NonTTY_Lines_Format(t *testing.T) {
	var buf bytes.Buffer
	sp := newSpinner(&buf, false)

	sp.Start("Checking")
	sp.Done("OK")

	// TrimRight only strips trailing newlines — preserves leading spaces.
	lines := strings.Split(strings.TrimRight(buf.String(), "\r\n"), "\n")
	require.Len(t, lines, 2)
	require.Equal(t, "  Checking...", strings.TrimRight(lines[0], "\r"))
	require.Equal(t, "  ✓ OK", strings.TrimRight(lines[1], "\r"))
}
