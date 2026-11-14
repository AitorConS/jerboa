package pkg

import (
	"errors"
	"io"
)

// ErrTruncatedArchive is returned for a tar that stops without its end
// marker.
var ErrTruncatedArchive = errors.New("archive is cut short: it ends without the tar end marker")

// tarEnd is the size of the marker that ends a tar: two blocks of zeroes.
const tarEnd = 1024

// endChecked reads a tar stream and remembers how it ends. archive/tar
// accepts a stream that stops between two entries as a complete, smaller
// archive; a package cut there would extract without an error and with files
// missing. A whole tar ends with the marker, which a stream cut at any point
// before it does not (short of a file that itself ends in a kilobyte of
// zeroes at a block boundary, which the archive's digest still catches).
type endChecked struct {
	r    io.Reader
	n    int64
	tail []byte
}

func (e *endChecked) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.n += int64(n)
	if n >= tarEnd {
		e.tail = append(e.tail[:0], p[n-tarEnd:n]...)
	} else if n > 0 {
		e.tail = append(e.tail, p[:n]...)
		if len(e.tail) > tarEnd {
			e.tail = append(e.tail[:0], e.tail[len(e.tail)-tarEnd:]...)
		}
	}
	return n, err //nolint:wrapcheck // a reader passes its source's errors through
}

// complete reads whatever follows the last entry and reports
// ErrTruncatedArchive unless the stream ended with the tar end marker.
func (e *endChecked) complete() error {
	if _, err := io.Copy(io.Discard, e); err != nil {
		return err //nolint:wrapcheck // callers name the archive
	}
	if e.n < tarEnd || e.n%512 != 0 || len(e.tail) < tarEnd {
		return ErrTruncatedArchive
	}
	for _, b := range e.tail {
		if b != 0 {
			return ErrTruncatedArchive
		}
	}
	return nil
}
