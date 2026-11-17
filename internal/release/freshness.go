package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/AitorConS/jerboa/internal/durable"
)

// stateFile records, per release origin and channel, the generation time of
// the newest manifest this machine has accepted.
const stateFile = "release-state.json"

// MinStableGenerated is the oldest stable manifest generation time this
// binary accepts, whatever its local state: a machine that has never fetched
// a manifest (or whose state was lost) still refuses an older one.
//
// Release builds do not use the value written here: the candidate workflow
// reads the generation time of the stable manifest that is live when the
// candidate is built and sets it with -X in every jerboa and jerboad it
// compiles; assembly refuses a binary without it and promotion refuses a
// candidate whose value a later release has overtaken (see
// docs/release-process.md). The value below is what a build from source
// gets: the commit time of the engine of v0.59.0, before which no stable
// manifest of that release or a later one can have been generated. It may be
// raised to the engine commit time of a newer promoted release, never beyond
// the live manifest's generation time (the candidate workflow refuses that).
var MinStableGenerated = "2026-11-13T18:27:12Z"

// InstallerStateFile is where scripts/install.sh records the stable manifest
// it installed from, in the same format as stateFile. Default reads it as a
// further floor; it is never written by the Go client.
const InstallerStateFile = "/etc/jerboa/release-state.json"

// checkFresh refuses a manifest older than one already accepted for the same
// origin and channel, so a validly signed but superseded manifest (and the
// older, possibly flawed, components it names) cannot be replayed. For the
// stable channel it also refuses one older than MinGenerated or than the
// installer's record (SeedStateFiles), and then requires a generation time.
// Otherwise a manifest without a generation time is accepted only while none
// with one has been seen. The newest time is then recorded in StateDir.
func (c *Client) checkFresh(m *Manifest) error {
	key := c.base() + " " + m.Channel
	path := filepath.Join(c.StateDir, stateFile)
	seen := map[string]string{}
	if c.StateDir != "" {
		var err error
		if seen, err = readState(path); err != nil {
			return err
		}
	}
	last, ok := seen[key]
	floor, floorFrom := c.floor(m.Channel, key)
	if m.Generated == "" {
		if ok {
			return fmt.Errorf("release: %s manifest has no generation time, but one generated at %s was already accepted", m.Channel, last)
		}
		if floorFrom != "" {
			return fmt.Errorf("release: %s manifest has no generation time, which %s requires", m.Channel, floorFrom)
		}
		return nil
	}
	generated, err := time.Parse(time.RFC3339, m.Generated)
	if err != nil {
		return fmt.Errorf("release: %s manifest: invalid generation time %q", m.Channel, m.Generated)
	}
	if floorFrom != "" && generated.Before(floor) {
		return fmt.Errorf("release: %s manifest generated at %s is older than %s allows (%s)", m.Channel, m.Generated, floorFrom, floor.Format(time.RFC3339))
	}
	if c.StateDir == "" {
		return nil
	}
	if ok {
		lastTime, err := time.Parse(time.RFC3339, last)
		if err == nil && generated.Before(lastTime) {
			return fmt.Errorf("release: %s manifest generated at %s is older than one already accepted (%s)", m.Channel, m.Generated, last)
		}
		if err == nil && !generated.After(lastTime) {
			return nil
		}
	}
	seen[key] = m.Generated
	if err := writeState(path, seen); err != nil {
		// Failing to record only weakens the next check; the manifest is valid.
		slog.Warn("could not record the release manifest generation time", "path", path, "err", err)
	}
	return nil
}

// floor returns the newest generation time a channel's manifest must not be
// older than regardless of StateDir, and what sets it ("" when nothing does).
// Only the stable channel has one: MinGenerated, or the installer's record
// for the same origin when that is newer. An unreadable or malformed record
// is ignored, as the installer writes it and it only raises the floor.
func (c *Client) floor(channel, key string) (time.Time, string) {
	if channel != ChannelStable {
		return time.Time{}, ""
	}
	var floor time.Time
	from := ""
	if c.MinGenerated != "" {
		if t, err := time.Parse(time.RFC3339, c.MinGenerated); err == nil {
			floor, from = t, "this build"
		}
	}
	for _, path := range c.SeedStateFiles {
		seen, err := readState(path)
		if err != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339, seen[key]); err == nil && t.After(floor) {
			floor, from = t, "the installer's record in "+path
		}
	}
	return floor, from
}

func readState(path string) (map[string]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the client's own state directory
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("release: read %s: %w", path, err)
	}
	seen := map[string]string{}
	if err := json.Unmarshal(data, &seen); err != nil {
		return nil, fmt.Errorf("release: %s is corrupt (delete it to reset): %w", path, err)
	}
	return seen, nil
}

func writeState(path string, seen map[string]string) error {
	data, err := json.MarshalIndent(seen, "", "  ")
	if err != nil {
		return fmt.Errorf("release: encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	// A truncated state file would make every later manifest fetch fail, so
	// it is replaced atomically and flushed.
	if err := durable.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("release: write state: %w", err)
	}
	return nil
}
