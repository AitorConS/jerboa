package api

import (
	"encoding/binary"
	"fmt"
	"io"
)

// maxFrameSize caps a single stream frame to guard against a malformed or
// hostile length header. Build-context chunks are written in small pieces
// (io.Copy uses 32 KiB), so this ceiling is never reached in practice.
const maxFrameSize = 64 << 20 // 64 MiB

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
	if len(p) == 0 {
		return 0, nil
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(p)))
	if _, err := fw.w.Write(hdr[:]); err != nil {
		return 0, fmt.Errorf("frame write header: %w", err)
	}
	if _, err := fw.w.Write(p); err != nil {
		return 0, fmt.Errorf("frame write payload: %w", err)
	}
	return len(p), nil
}

// Close writes the zero-length terminator frame. It does not close the
// underlying writer.
func (fw *frameWriter) Close() error {
	var hdr [4]byte // length 0
	if _, err := fw.w.Write(hdr[:]); err != nil {
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
			return 0, fmt.Errorf("frame too large: %d bytes (max %d)", n, maxFrameSize)
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
