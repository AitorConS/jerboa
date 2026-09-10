//go:build linux || (darwin && arm64)

package vm

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

// copyBootImage hashes exactly the bytes written to the private boot image.
// This avoids reading the shared image for verification and then reading it
// again to copy, which would also allow source changes between those operations.
func copyBootImage(dst, src, digest string) (err error) {
	if digest == "" {
		return copyFile(dst, src)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open boot image: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create boot copy: %w", err)
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	h := sha256.New()
	if _, err = io.Copy(io.MultiWriter(out, h), in); err != nil {
		return fmt.Errorf("copy boot image: %w", err)
	}
	if fmt.Sprintf("sha256:%x", h.Sum(nil)) != digest {
		return fmt.Errorf("boot image integrity mismatch")
	}
	return nil
}
