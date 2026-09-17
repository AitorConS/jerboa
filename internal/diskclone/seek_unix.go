//go:build linux || darwin

package diskclone

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// forEachDataRange calls fn for each [start, end) range of f that may hold
// data, in ascending order, using SEEK_DATA/SEEK_HOLE. Filesystems without
// hole reporting yield a single range covering the whole file.
func forEachDataRange(f *os.File, size int64, fn func(start, end int64) error) error {
	fd := int(f.Fd())
	for off := int64(0); off < size; {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				return nil // no data after off: the rest is a hole
			}
			if off == 0 && (errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)) {
				return fn(0, size)
			}
			return fmt.Errorf("diskclone: seek data: %w", err)
		}
		if data >= size {
			return nil
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			return fmt.Errorf("diskclone: seek hole: %w", err)
		}
		if hole > size {
			hole = size
		}
		if err := fn(data, hole); err != nil {
			return err
		}
		off = hole
	}
	return nil
}
