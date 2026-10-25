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
)

// stateFile records, per release origin and channel, the generation time of
// the newest manifest this machine has accepted.
const stateFile = "release-state.json"

// checkFresh refuses a manifest older than one already accepted for the same
// origin and channel, so a validly signed but superseded manifest (and the
// older, possibly flawed, components it names) cannot be replayed. A manifest
// without a generation time is accepted only while none with one has been
// seen. The newest time is then recorded.
func (c *Client) checkFresh(m *Manifest) error {
	if c.StateDir == "" {
		return nil
	}
	key := c.base() + " " + m.Channel
	path := filepath.Join(c.StateDir, stateFile)
	seen, err := readState(path)
	if err != nil {
		return err
	}
	last, ok := seen[key]
	if m.Generated == "" {
		if ok {
			return fmt.Errorf("release: %s manifest has no generation time, but one generated at %s was already accepted", m.Channel, last)
		}
		return nil
	}
	generated, err := time.Parse(time.RFC3339, m.Generated)
	if err != nil {
		return fmt.Errorf("release: %s manifest: invalid generation time %q", m.Channel, m.Generated)
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	tmp, err := os.CreateTemp(dir, stateFile+".*")
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("release: write state: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("release: %w", err)
	}
	return nil
}
