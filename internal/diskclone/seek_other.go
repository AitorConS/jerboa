//go:build !linux && !darwin

package diskclone

import "os"

// forEachDataRange reports the whole file as data where hole reporting is
// unavailable; SparseCopy still skips all-zero blocks.
func forEachDataRange(_ *os.File, size int64, fn func(start, end int64) error) error {
	if size == 0 {
		return nil
	}
	return fn(0, size)
}
