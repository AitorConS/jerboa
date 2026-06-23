package api

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func TestFraming_RoundTrip(t *testing.T) {
	sizes := []int{0, 1, 100, 32 * 1024, 200 * 1024}
	for _, size := range sizes {
		payload := make([]byte, size)
		if _, err := rand.Read(payload); err != nil {
			t.Fatalf("rand: %v", err)
		}

		var wire bytes.Buffer
		fw := newFrameWriter(&wire)
		if _, err := io.Copy(fw, bytes.NewReader(payload)); err != nil {
			t.Fatalf("write frames: %v", err)
		}
		if err := fw.Close(); err != nil {
			t.Fatalf("close frames: %v", err)
		}

		// A trailing sentinel proves the reader stops exactly at the terminator.
		wire.WriteString("SENTINEL")

		fr := newFrameReader(&wire)
		got, err := io.ReadAll(fr)
		if err != nil {
			t.Fatalf("read frames (size %d): %v", size, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("size %d: payload mismatch (got %d bytes)", size, len(got))
		}
		rest, _ := io.ReadAll(&wire)
		if string(rest) != "SENTINEL" {
			t.Fatalf("size %d: reader consumed past terminator, leftover=%q", size, rest)
		}
	}
}
