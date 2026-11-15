//go:build !unix

package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/AitorConS/jerboa/internal/durable"
)

// readPrivate reads path directly on systems without POSIX ownership. On
// Windows the temp directory is per user, so the fallback is not shared.
func readPrivate(_, path string) ([]byte, error) { return os.ReadFile(path) }

// writePrivate writes path directly, for the same reason.
func writePrivate(_, path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: create dir: %w", err)
	}
	if err := durable.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}
