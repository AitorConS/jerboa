//go:build linux || (darwin && arm64)

package apiserver

import (
	"archive/tar"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// tarWithFile builds a one-file tar archive of the given size.
func tarWithFile(t *testing.T, name string, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(size), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte("x"), size)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBuildContextEnforcesTotalLimit(t *testing.T) {
	orig := maxBuildContextBytes
	maxBuildContextBytes = 1 << 10 // 1 KiB
	t.Cleanup(func() { maxBuildContextBytes = orig })

	// A single file over the cap must be rejected, not truncated onto disk.
	over := tarWithFile(t, "big.bin", 4<<10)
	if _, err := extractBuildContext(bytes.NewReader(over), t.TempDir()); err == nil {
		t.Fatal("expected error for over-limit build context, got nil")
	}
}

func TestExtractBuildContextAccumulatesAcrossFiles(t *testing.T) {
	orig := maxBuildContextBytes
	maxBuildContextBytes = 1 << 10 // 1 KiB
	t.Cleanup(func() { maxBuildContextBytes = orig })

	// Two files each under the cap but together over it must still be rejected:
	// the limit is cumulative, not per-frame.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"a.bin", "b.bin"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 768, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(bytes.Repeat([]byte("y"), 768)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBuildContext(bytes.NewReader(buf.Bytes()), t.TempDir()); err == nil {
		t.Fatal("expected error for cumulative over-limit context, got nil")
	}
}

func TestExtractBuildContextUnderLimitPasses(t *testing.T) {
	small := tarWithFile(t, "ok.bin", 512)
	files, err := extractBuildContext(bytes.NewReader(small), t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
}

// tarOfEntries builds an archive of n entries of one type, each named by i.
func tarOfEntries(t *testing.T, n int, typ byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := range n {
		name := fmt.Sprintf("e%d", i)
		hdr := &tar.Header{Name: name, Mode: 0o644, Typeflag: typ}
		if typ == tar.TypeDir {
			hdr.Name += "/"
			hdr.Mode = 0o755
		}
		if typ == tar.TypeSymlink {
			hdr.Linkname = "target"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Directories, empty files and skipped entries wrote no content, so they cost
// nothing against the byte budget and a client could send them without end.
func TestExtractBuildContextChargesEveryHeader(t *testing.T) {
	orig := maxBuildContextBytes
	maxBuildContextBytes = 64 * tarHeaderCost
	t.Cleanup(func() { maxBuildContextBytes = orig })

	for name, typ := range map[string]byte{"directories": tar.TypeDir, "empty files": tar.TypeReg, "symlinks": tar.TypeSymlink} {
		t.Run(name, func(t *testing.T) {
			if _, err := extractBuildContext(bytes.NewReader(tarOfEntries(t, 65, typ)), t.TempDir()); err == nil || !strings.Contains(err.Error(), "byte limit") {
				t.Fatalf("expected the byte limit, got %v", err)
			}
			if _, err := extractBuildContext(bytes.NewReader(tarOfEntries(t, 64, typ)), t.TempDir()); err != nil {
				t.Fatalf("64 entries fit the budget: %v", err)
			}
		})
	}
}

func TestExtractBuildContextCapsEntries(t *testing.T) {
	orig := maxBuildContextEntries
	maxBuildContextEntries = 100
	t.Cleanup(func() { maxBuildContextEntries = orig })

	files, err := extractBuildContext(bytes.NewReader(tarOfEntries(t, 100, tar.TypeDir)), t.TempDir())
	if err != nil || len(files) != 100 {
		t.Fatalf("100 entries are allowed: %d files, %v", len(files), err)
	}
	// The same directory sent again and again grows the file list too.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for range 101 {
		if err := tw.WriteHeader(&tar.Header{Name: "same/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBuildContext(bytes.NewReader(buf.Bytes()), t.TempDir()); err == nil || !strings.Contains(err.Error(), "more than 100 entries") {
		t.Fatalf("expected the entry limit, got %v", err)
	}
}
