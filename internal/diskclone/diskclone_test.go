package diskclone

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeSparseImage writes size bytes to path with data only at the given
// offsets, leaving the rest as holes (on filesystems that support them).
func writeSparseImage(t *testing.T, path string, size int64, chunks map[int64][]byte) []byte {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	want := make([]byte, size)
	for off, data := range chunks {
		_, err := f.WriteAt(data, off)
		require.NoError(t, err)
		copy(want[off:], data)
	}
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
	return want
}

func randomBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}

func sparseFixture(t *testing.T, dir string) (string, []byte) {
	r := rand.New(rand.NewSource(1))
	src := filepath.Join(dir, "disk.img")
	// Uneven size, data at the start, straddling a block boundary, a zero-filled
	// written region and data right before the end.
	size := int64(8<<20 + 123)
	want := writeSparseImage(t, src, size, map[int64][]byte{
		0:                   randomBytes(r, 4096),
		blockSize - 100:     randomBytes(r, 300),
		2 << 20:             make([]byte, 3*blockSize), // explicit zeros
		size - 17:           randomBytes(r, 17),
		5<<20 + blockSize/2: randomBytes(r, blockSize),
	})
	return src, want
}

func TestCloneProducesIdenticalIndependentCopy(t *testing.T) {
	dir := t.TempDir()
	src, want := sparseFixture(t, dir)
	dst := filepath.Join(dir, "vm", "rootfs.img")
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o700))
	// A stale file from a crashed run must be replaced, not appended to.
	require.NoError(t, os.WriteFile(dst, []byte("stale contents that are longer than nothing"), 0o644))

	method, err := Clone(dst, src)
	require.NoError(t, err)
	require.Contains(t, []Method{MethodClone, MethodSparseCopy}, method)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, want, got)
	st, err := os.Stat(dst)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	f, err := os.OpenFile(dst, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("guest writes"), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	base, err := os.ReadFile(src)
	require.NoError(t, err)
	require.Equal(t, want, base, "writes to the clone must not reach the source")
}

func TestSparseCopyIdentical(t *testing.T) {
	dir := t.TempDir()
	src, want := sparseFixture(t, dir)
	dst := filepath.Join(dir, "copy.img")
	require.NoError(t, SparseCopy(dst, src))
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestSparseCopyEmptyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty.img")
	require.NoError(t, os.WriteFile(src, nil, 0o600))
	dst := filepath.Join(dir, "copy.img")
	require.NoError(t, SparseCopy(dst, src))
	st, err := os.Stat(dst)
	require.NoError(t, err)
	require.Zero(t, st.Size())
}

func TestCloneMissingSource(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "rootfs.img")
	_, err := Clone(dst, filepath.Join(dir, "missing.img"))
	require.Error(t, err)
	_, statErr := os.Stat(dst)
	require.ErrorIs(t, statErr, os.ErrNotExist, "failed clone must not leave a partial file")
}

func TestHashFileMatchesFullRead(t *testing.T) {
	dir := t.TempDir()
	src, want := sparseFixture(t, dir)
	got, err := HashFile(src)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(want)), got)

	empty := filepath.Join(dir, "empty.img")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	got, err = HashFile(empty)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(nil)), got)

	holeOnly := filepath.Join(dir, "hole.img")
	writeSparseImage(t, holeOnly, 3<<20+5, nil)
	got, err = HashFile(holeOnly)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(make([]byte, 3<<20+5))), got)
}
