//go:build linux || darwin

package diskclone

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	require.NoError(t, err)
	return st.Sys().(*syscall.Stat_t).Blocks * 512
}

// TestSparseCopyDoesNotAllocateHoles guards the main point of the package: a
// large, mostly empty image must not be materialized on every VM start.
func TestSparseCopyDoesNotAllocateHoles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "disk.img")
	size := int64(256 << 20)
	writeSparseImage(t, src, size, map[int64][]byte{0: make([]byte, 1<<20), 1 << 20: []byte("data")})
	if allocatedBytes(t, src) >= size/2 {
		t.Skip("filesystem does not support sparse files")
	}
	dst := filepath.Join(dir, "copy.img")
	require.NoError(t, SparseCopy(dst, src))
	require.Less(t, allocatedBytes(t, dst), int64(8<<20))
}
