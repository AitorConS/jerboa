package api

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// maxFrameSize caps a single stream frame to guard against a malformed or
// hostile length header. Build-context chunks are written in small pieces
// (io.Copy uses 32 KiB), so this ceiling is never reached in practice.
const maxFrameSize = 64 << 20 // 64 MiB

// ErrFrameTooLarge is returned when a frame's length header exceeds
// maxFrameSize. It is a sentinel so callers on the untrusted RPC surface can
// distinguish a hostile length prefix from a transport error with errors.Is.
var ErrFrameTooLarge = errors.New("frame too large")

// frameWriter writes a byte stream as length-prefixed frames. Each Write emits
// one frame (a 4-byte big-endian length followed by the payload). Close emits a
// zero-length terminator frame so the reader can detect end-of-stream without
// closing the underlying connection.
//
// This lets a large build context stream over a persistent JSON-RPC connection
// without buffering the whole archive or precomputing its size.
type frameWriter struct {
	w io.Writer
}

func NewFrameWriter(w io.Writer) *frameWriter { return &frameWriter{w: w} }

func (fw *frameWriter) Write(p []byte) (int, error) {
	written := 0
	// A payload above maxFrameSize goes out as several frames: the reader
	// refuses a longer one, and a source that hands its whole content over in
	// one Write (io.Copy from a bytes.Reader does) must not produce a stream
	// its peer rejects.
	for len(p) > 0 {
		chunk := p[:min(len(p), maxFrameSize)]
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(chunk)))
		if err := writeFull(fw.w, hdr[:]); err != nil {
			return written, fmt.Errorf("frame write header: %w", err)
		}
		if err := writeFull(fw.w, chunk); err != nil {
			return written, fmt.Errorf("frame write payload: %w", err)
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

// writeFull writes all of b. A writer that takes fewer bytes without saying
// why would leave the stream cut inside a frame with nobody told, so that is
// an error here.
func writeFull(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err != nil {
		return err //nolint:wrapcheck // wrapped by the caller, which names the part of the frame
	}
	if n < len(b) {
		return io.ErrShortWrite
	}
	return nil
}

// Close writes the zero-length terminator frame. It does not close the
// underlying writer.
func (fw *frameWriter) Close() error {
	var hdr [4]byte // length 0
	if err := writeFull(fw.w, hdr[:]); err != nil {
		return fmt.Errorf("frame write terminator: %w", err)
	}
	return nil
}

// frameReader reassembles the byte stream produced by a frameWriter. It returns
// io.EOF once the terminator frame is read, leaving the underlying reader
// positioned for the next protocol message.
type frameReader struct {
	r         io.Reader
	remaining uint32
	done      bool
}

func NewFrameReader(r io.Reader) *frameReader { return &frameReader{r: r} }

func (fr *frameReader) Read(p []byte) (int, error) {
	if fr.done {
		return 0, io.EOF
	}
	if fr.remaining == 0 {
		var hdr [4]byte
		if _, err := io.ReadFull(fr.r, hdr[:]); err != nil {
			return 0, fmt.Errorf("frame read header: %w", err)
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 {
			fr.done = true
			return 0, io.EOF
		}
		if n > maxFrameSize {
			return 0, fmt.Errorf("%w: %d bytes (max %d)", ErrFrameTooLarge, n, maxFrameSize)
		}
		fr.remaining = n
	}
	toRead := len(p)
	if uint32(toRead) > fr.remaining {
		toRead = int(fr.remaining)
	}
	n, err := fr.r.Read(p[:toRead])
	fr.remaining -= uint32(n)
	if err != nil {
		return n, fmt.Errorf("frame read payload: %w", err)
	}
	return n, nil
}
