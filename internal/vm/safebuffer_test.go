//go:build linux

package vm

import (
	"bytes"
	"strings"
	"testing"
)

func TestSafeBufferBounded(t *testing.T) {
	b := &safeBuffer{}
	b.max = 100

	// Write far more than max in small chunks.
	for range 1000 {
		if _, err := b.Write([]byte("0123456789")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got := len(b.buf); got > b.max {
			t.Fatalf("len(buf)=%d exceeds max=%d", got, b.max)
		}
		if got := cap(b.buf); got > b.max {
			t.Fatalf("cap(buf)=%d exceeds max=%d", got, b.max)
		}
	}
	out := b.Bytes()
	if len(out) != b.max {
		t.Fatalf("retained %d bytes, want %d", len(out), b.max)
	}
	// Retained content must be the most recent bytes, in order.
	if !bytes.Equal(out, []byte(strings.Repeat("0123456789", 10))) {
		t.Fatalf("unexpected tail content: %q", out)
	}
}

func TestSafeBufferSingleChunkLargerThanMax(t *testing.T) {
	b := &safeBuffer{}
	b.max = 8
	if _, err := b.Write([]byte("abcdefghXYZ")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(b.Bytes()); got != "defghXYZ" {
		t.Fatalf("got %q, want tail %q", got, "defghXYZ")
	}
	if cap(b.buf) > b.max {
		t.Fatalf("cap(buf)=%d exceeds max=%d", cap(b.buf), b.max)
	}
}

func TestSetVMLogMaxBytesZeroRestoresDefault(t *testing.T) {
	t.Cleanup(func() { vmLogMaxBytes.Store(defaultVMLogMaxBytes) })
	SetVMLogMaxBytes(1234)
	if got := vmLogMaxBytes.Load(); got != 1234 {
		t.Fatalf("got %d, want 1234", got)
	}
	SetVMLogMaxBytes(0)
	if got := vmLogMaxBytes.Load(); got != defaultVMLogMaxBytes {
		t.Fatalf("0 did not restore default: got %d", got)
	}
}
