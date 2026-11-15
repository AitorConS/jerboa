//go:build !unix

package config

import "os"

// readPrivate reads path directly on systems without POSIX ownership. On
// Windows the temp directory is per user, so the fallback is not shared.
func readPrivate(_, path string) ([]byte, error) { return os.ReadFile(path) }
