//go:build linux || (darwin && arm64)

package vm

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCopyBootImageIntegrity(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "boot")
	original := []byte("verified image")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(original))
	require.NoError(t, os.WriteFile(src, original, 0o600))
	require.NoError(t, copyBootImage(dst, src, digest))
	data, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, original, data)
	// A source changed after reference resolution must never become bootable.
	require.NoError(t, os.WriteFile(src, []byte("tampered image"), 0o600))
	require.ErrorContains(t, copyBootImage(dst, src, digest), "integrity mismatch")
	_, err = os.Stat(dst)
	require.ErrorIs(t, err, os.ErrNotExist)
}
