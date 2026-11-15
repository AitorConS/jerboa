package pkg

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

// IndexPurpose is the purpose a package index declares. Together with the
// signature's trusted comment it keeps another document signed with the
// release key from being read as the index.
const IndexPurpose = "jerboa-package-index"

// indexStateFile records, per index URL, the serial of the newest index this
// store has accepted. It lives next to the cached index.json.
const indexStateFile = "index-state.json"

// checkIndexFresh refuses an index that is older than one already accepted
// from the same URL, so a superseded but validly signed packages.json (and
// the older versions it ranks first) cannot be replayed. The newest serial is
// recorded in dir; with dir empty only the index's own fields are checked.
//
// Indexes published before the serial existed carry neither purpose nor
// serial. They are accepted, with a warning, only while no serial has been
// recorded for the URL. This compatibility path is deprecated and goes away
// once every published index carries a serial.
func checkIndexFresh(dir, indexURL string, idx *Index) error {
	path := filepath.Join(dir, indexStateFile)
	seen := map[string]uint64{}
	if dir != "" {
		var err error
		if seen, err = readIndexState(path); err != nil {
			return err
		}
	}
	floor, recorded := seen[indexURL]
	if idx.Purpose == "" && idx.Serial == 0 {
		if recorded {
			return fmt.Errorf("package index has no serial, but serial %d was already accepted from %s", floor, indexURL)
		}
		slog.Warn("package index has no serial; accepting it because none was accepted from this URL before (deprecated)", "url", indexURL)
		return nil
	}
	if idx.Purpose != IndexPurpose {
		return fmt.Errorf("package index: purpose %q, want %q", idx.Purpose, IndexPurpose)
	}
	if idx.Serial == 0 {
		return errors.New("package index: missing serial")
	}
	if idx.Generated != "" {
		if _, err := time.Parse(time.RFC3339, idx.Generated); err != nil {
			return fmt.Errorf("package index: invalid generation time %q", idx.Generated)
		}
	}
	if idx.Serial < floor {
		return fmt.Errorf("package index serial %d is older than one already accepted from %s (%d)", idx.Serial, indexURL, floor)
	}
	if dir == "" || (recorded && idx.Serial == floor) {
		return nil
	}
	seen[indexURL] = idx.Serial
	if err := writeIndexState(path, seen); err != nil {
		// Failing to record only weakens the next check; the index is valid.
		slog.Warn("could not record the package index serial", "path", path, "err", err)
	}
	return nil
}

func readIndexState(path string) (map[string]uint64, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]uint64{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("package index state: %w", err)
	}
	seen := map[string]uint64{}
	if err := json.Unmarshal(data, &seen); err != nil {
		return nil, fmt.Errorf("package index state %s is corrupt (delete it to reset): %w", path, err)
	}
	return seen, nil
}

func writeIndexState(path string, seen map[string]uint64) error {
	data, err := json.MarshalIndent(seen, "", "  ")
	if err != nil {
		return fmt.Errorf("package index state: %w", err)
	}
	if err := durable.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("package index state: %w", err)
	}
	return nil
}
