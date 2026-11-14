package diskclone

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What must hold when a private disk copy cannot be made or is made from
// something that is not a plain image: the source image is never changed or
// removed, no partial destination is left for a VM to boot from, and a copy
// that is reported as made reads back exactly like its source.

// withoutClone makes the filesystem refuse copy-on-write clones, as ext4,
// tmpfs and cross-device paths do, so Clone has to copy.
func withoutClone(t *testing.T) {
	t.Helper()
	prev := tryClone
	tryClone = func(_, _ string) bool { return false }
	t.Cleanup(func() { tryClone = prev })
}

// methods runs a test once with the platform's clone and once without it.
func methods(t *testing.T, run func(t *testing.T)) {
	t.Helper()
	t.Run("clone", run)
	t.Run("copy", func(t *testing.T) {
		withoutClone(t)
		run(t)
	})
}

func requireGone(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	require.True(t, os.IsNotExist(err), "%s was left behind (%v)", path, err)
}

func skipIfPermissionsDoNotApply(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
}

func TestFaults_CloneUnsupportedFallsBackToAFaithfulCopy(t *testing.T) {
	withoutClone(t)
	dir := t.TempDir()
	src, want := sparseFixture(t, dir)
	dst := filepath.Join(dir, "rootfs.img")

	method, err := Clone(dst, src)
	require.NoError(t, err)
	require.Equal(t, MethodSparseCopy, method)
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.True(t, bytes.Equal(want, got), "the copy differs from its source")

	// The digest a VM start verifies is the source's.
	srcHash, err := HashFile(src)
	require.NoError(t, err)
	dstHash, err := HashFile(dst)
	require.NoError(t, err)
	assert.Equal(t, srcHash, dstHash)
	assert.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(want)), dstHash)
}

func TestFaults_SourceMissingLeavesNoDestination(t *testing.T) {
	methods(t, func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "rootfs.img")
		for _, src := range []string{
			filepath.Join(dir, "missing.img"),
			filepath.Join(dir, "no-such-dir", "disk.img"),
			"",
		} {
			_, err := Clone(dst, src)
			require.Error(t, err, "source %q", src)
			requireGone(t, dst)
		}
		// A copy left by an earlier run does not survive either: a VM must
		// not boot from a disk that is not the image it asked for.
		require.NoError(t, os.WriteFile(dst, []byte("stale"), 0o600))
		_, err := Clone(dst, filepath.Join(dir, "missing.img"))
		require.Error(t, err)
		requireGone(t, dst)
	})
}

func TestFaults_SourceIsNotAnImage(t *testing.T) {
	methods(t, func(t *testing.T) {
		dir := t.TempDir()
		srcDir := filepath.Join(dir, "images")
		require.NoError(t, os.Mkdir(srcDir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "disk.img"), []byte("data"), 0o600))
		dst := filepath.Join(dir, "rootfs.img")

		// A directory is not a disk; cloning it must not produce a
		// directory where the VM expects its root filesystem.
		_, err := Clone(dst, srcDir)
		require.Error(t, err)
		requireGone(t, dst)
		data, err := os.ReadFile(filepath.Join(srcDir, "disk.img"))
		require.NoError(t, err)
		assert.Equal(t, "data", string(data))
	})
}

func TestFaults_SourceUnreadable(t *testing.T) {
	skipIfPermissionsDoNotApply(t)
	methods(t, func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "disk.img")
		require.NoError(t, os.WriteFile(src, []byte("secret image"), 0o000))
		dst := filepath.Join(dir, "rootfs.img")

		_, err := Clone(dst, src)
		require.Error(t, err)
		requireGone(t, dst)
		_, err = HashFile(src)
		require.Error(t, err)
	})
}

func TestFaults_DestinationUnwritable(t *testing.T) {
	skipIfPermissionsDoNotApply(t)
	methods(t, func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "disk.img")
		require.NoError(t, os.WriteFile(src, []byte("image"), 0o600))

		t.Run("directory missing", func(t *testing.T) {
			dst := filepath.Join(dir, "no-such-dir", "rootfs.img")
			_, err := Clone(dst, src)
			require.Error(t, err)
			requireGone(t, filepath.Dir(dst))
		})
		t.Run("directory read-only", func(t *testing.T) {
			locked := filepath.Join(dir, "locked")
			require.NoError(t, os.Mkdir(locked, 0o500))
			dst := filepath.Join(locked, "rootfs.img")
			_, err := Clone(dst, src)
			require.Error(t, err)
			requireGone(t, dst)
		})
		t.Run("stale copy cannot be replaced", func(t *testing.T) {
			locked := filepath.Join(dir, "held")
			require.NoError(t, os.Mkdir(locked, 0o700))
			dst := filepath.Join(locked, "rootfs.img")
			require.NoError(t, os.WriteFile(dst, []byte("stale"), 0o600))
			require.NoError(t, os.Chmod(locked, 0o500))
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: a directory needs its search bit.

			// The old copy cannot be removed, so the new one cannot be
			// made: that is an error, not a silent reuse of the old disk.
			_, err := Clone(dst, src)
			require.ErrorContains(t, err, "remove stale")
		})
		t.Run("destination is a directory with content", func(t *testing.T) {
			dst := filepath.Join(dir, "occupied")
			require.NoError(t, os.Mkdir(dst, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dst, "keep"), []byte("x"), 0o600))
			_, err := Clone(dst, src)
			require.Error(t, err)
			_, err = os.Stat(filepath.Join(dst, "keep"))
			require.NoError(t, err, "a directory in the way lost its content")
		})

		// The image survived every failed attempt.
		data, err := os.ReadFile(src)
		require.NoError(t, err)
		assert.Equal(t, "image", string(data))
	})
}

func TestFaults_DestinationIsTheSourceItself(t *testing.T) {
	methods(t, func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "disk.img")
		require.NoError(t, os.WriteFile(src, []byte("the only copy"), 0o600))

		// Replacing the destination must not start by deleting the image.
		for _, dst := range []string{src, filepath.Join(dir, ".", "disk.img"), filepath.Join(dir, "sub", "..", "disk.img")} {
			_, err := Clone(dst, src)
			require.Error(t, err, "destination %q", dst)
			data, rerr := os.ReadFile(src)
			require.NoError(t, rerr, "the source image was removed")
			require.Equal(t, "the only copy", string(data))
		}
	})
}

func TestFaults_SymlinksNeverLeadTheCopyAstray(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	methods(t, func(t *testing.T) {
		t.Run("source is a symlink", func(t *testing.T) {
			dir := t.TempDir()
			image := filepath.Join(dir, "disk-v1.img")
			require.NoError(t, os.WriteFile(image, []byte("image content"), 0o644)) //nolint:gosec // G306: the mode is what the test is about.
			link := filepath.Join(dir, "current.img")
			require.NoError(t, os.Symlink(image, link))
			dst := filepath.Join(dir, "rootfs.img")

			_, err := Clone(dst, link)
			require.NoError(t, err)
			// The copy is a file of its own, not a second name for the
			// image: the VM writing to its disk must not write the image.
			info, err := os.Lstat(dst)
			require.NoError(t, err)
			require.True(t, info.Mode().IsRegular(), "the copy is a %s", info.Mode().Type())
			require.NoError(t, os.WriteFile(dst, []byte("guest wrote here"), 0o600))
			data, err := os.ReadFile(image)
			require.NoError(t, err)
			assert.Equal(t, "image content", string(data), "a write to the copy changed the image")
			// Nor did making the copy change the image's own mode.
			info, err = os.Stat(image)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
		})
		t.Run("source is a dangling symlink", func(t *testing.T) {
			dir := t.TempDir()
			link := filepath.Join(dir, "current.img")
			require.NoError(t, os.Symlink(filepath.Join(dir, "gone.img"), link))
			dst := filepath.Join(dir, "rootfs.img")
			_, err := Clone(dst, link)
			require.Error(t, err)
			requireGone(t, dst)
		})
		t.Run("destination is a symlink to a bystander", func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "disk.img")
			require.NoError(t, os.WriteFile(src, []byte("image content"), 0o600))
			bystander := filepath.Join(dir, "precious")
			require.NoError(t, os.WriteFile(bystander, []byte("do not touch"), 0o600))
			dst := filepath.Join(dir, "rootfs.img")
			require.NoError(t, os.Symlink(bystander, dst))

			_, err := Clone(dst, src)
			require.NoError(t, err)
			data, err := os.ReadFile(bystander)
			require.NoError(t, err)
			assert.Equal(t, "do not touch", string(data), "the copy was written through a planted symlink")
			info, err := os.Lstat(dst)
			require.NoError(t, err)
			require.True(t, info.Mode().IsRegular())
			data, err = os.ReadFile(dst)
			require.NoError(t, err)
			assert.Equal(t, "image content", string(data))
		})
		t.Run("destination is a symlink to the source", func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "disk.img")
			require.NoError(t, os.WriteFile(src, []byte("image content"), 0o600))
			dst := filepath.Join(dir, "rootfs.img")
			require.NoError(t, os.Symlink(src, dst))

			_, err := Clone(dst, src)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(dst, []byte("guest wrote here"), 0o600))
			data, err := os.ReadFile(src)
			require.NoError(t, err)
			assert.Equal(t, "image content", string(data))
		})
	})
}

func TestFaults_CopyIsPrivateWhateverTheSourceMode(t *testing.T) {
	skipIfPermissionsDoNotApply(t)
	methods(t, func(t *testing.T) {
		for _, mode := range []os.FileMode{0o400, 0o444, 0o644, 0o666, 0o755} {
			dir := t.TempDir()
			src := filepath.Join(dir, "disk.img")
			require.NoError(t, os.WriteFile(src, []byte("image"), 0o600))
			require.NoError(t, os.Chmod(src, mode))
			dst := filepath.Join(dir, "rootfs.img")
			// A stale copy with loose permissions does not pass them on.
			require.NoError(t, os.WriteFile(dst, []byte("stale"), 0o666)) //nolint:gosec // G306: the mode is what the test is about.

			_, err := Clone(dst, src)
			require.NoError(t, err, "source mode %o", mode)
			info, err := os.Stat(dst)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "source mode %o", mode)
			// The VM can write its disk even when the image is read-only,
			// and the image keeps its mode and content.
			require.NoError(t, os.WriteFile(dst, []byte("guest"), 0o600))
			info, err = os.Stat(src)
			require.NoError(t, err)
			assert.Equal(t, mode, info.Mode().Perm())
			require.NoError(t, os.Chmod(src, 0o600))
			data, err := os.ReadFile(src)
			require.NoError(t, err)
			assert.Equal(t, "image", string(data))
		}
	})
}

func TestFaults_CopyAndSourceAreIndependentBothWays(t *testing.T) {
	methods(t, func(t *testing.T) {
		dir := t.TempDir()
		src, want := sparseFixture(t, dir)
		dst := filepath.Join(dir, "rootfs.img")
		_, err := Clone(dst, src)
		require.NoError(t, err)

		// The image is replaced or rewritten (a pull, a rebuild) while a VM
		// runs from its copy: the VM's disk does not change under it.
		f, err := os.OpenFile(src, os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteAt(bytes.Repeat([]byte{0xee}, 2*blockSize), 100)
		require.NoError(t, err)
		require.NoError(t, f.Truncate(4096))
		require.NoError(t, f.Close())
		got, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.True(t, bytes.Equal(want, got), "the copy changed when its source did")

		require.NoError(t, os.Remove(src))
		got, err = os.ReadFile(dst)
		require.NoError(t, err)
		require.True(t, bytes.Equal(want, got), "the copy did not survive the removal of its source")
	})
}

func TestFaults_SparseCopyAtBlockBoundaries(t *testing.T) {
	// Sizes and data positions around the block the copy works in: an
	// off-by-one there loses the last bytes of an image or its length.
	sizes := []int64{1, blockSize - 1, blockSize, blockSize + 1, 2*blockSize - 1, 2 * blockSize, 3*blockSize + 7}
	for _, size := range sizes {
		layouts := map[string]map[int64][]byte{
			"all holes":       {},
			"first byte only": {0: {0x01}},
			"last byte only":  {size - 1: {0x01}},
			"all data":        {0: bytes.Repeat([]byte{0xab}, int(size))},
			"written zeroes":  {0: make([]byte, size)},
		}
		if size > blockSize {
			layouts["across the boundary"] = map[int64][]byte{blockSize - 1: {0x01, 0x02}}
			layouts["zero block then data"] = map[int64][]byte{0: make([]byte, blockSize), blockSize: {0x07}}
		}
		for name, chunks := range layouts {
			t.Run(fmt.Sprintf("%d/%s", size, name), func(t *testing.T) {
				dir := t.TempDir()
				src := filepath.Join(dir, "disk.img")
				want := writeSparseImage(t, src, size, chunks)
				dst := filepath.Join(dir, "copy.img")
				require.NoError(t, SparseCopy(dst, src))

				got, err := os.ReadFile(dst)
				require.NoError(t, err)
				require.Len(t, got, len(want), "the copy has another size")
				require.True(t, bytes.Equal(want, got))
				for _, path := range []string{src, dst} {
					sum, err := HashFile(path)
					require.NoError(t, err)
					require.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(want)), sum, path)
				}
			})
		}
	}
}

func TestFaults_SparseCopyRandomLayouts(t *testing.T) {
	r := rand.New(rand.NewSource(7)) //nolint:gosec // G404: reproducible test data.
	for i := range 25 {
		size := int64(1 + r.Intn(6*blockSize))
		chunks := map[int64][]byte{}
		for range r.Intn(6) {
			off := r.Int63n(size)
			n := 1 + r.Int63n(min(size-off, 2*blockSize))
			data := randomBytes(r, int(n))
			if r.Intn(3) == 0 {
				data = make([]byte, n) // zeros written on purpose
			}
			chunks[off] = data
		}
		dir := t.TempDir()
		src := filepath.Join(dir, "disk.img")
		// Chunks may overlap and a map has no order, so the truth is what
		// the file reads back as.
		writeSparseImage(t, src, size, chunks)
		want, err := os.ReadFile(src)
		require.NoError(t, err)

		dst := filepath.Join(dir, "copy.img")
		require.NoError(t, SparseCopy(dst, src), "layout %d", i)
		got, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.True(t, bytes.Equal(want, got), "layout %d (size %d): the copy differs", i, size)
		sum, err := HashFile(dst)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(want)), sum, "layout %d", i)
	}
}

func TestFaults_HashFile(t *testing.T) {
	dir := t.TempDir()
	_, err := HashFile(filepath.Join(dir, "missing"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = HashFile(dir)
	require.Error(t, err, "a directory has no digest")

	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	sum, err := HashFile(empty)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(nil)), sum)

	// One flipped bit anywhere, a missing byte or an extra one: the digest
	// that guards a VM's boot disk notices each.
	src, want := sparseFixture(t, dir)
	base, err := HashFile(src)
	require.NoError(t, err)
	for name, damage := range map[string]func(f *os.File){
		"bit in data":     func(f *os.File) { _, _ = f.WriteAt([]byte{want[10] ^ 1}, 10) },
		"bit in a hole":   func(f *os.File) { _, _ = f.WriteAt([]byte{1}, 7<<20) },
		"last byte":       func(f *os.File) { _, _ = f.WriteAt([]byte{want[len(want)-1] ^ 0x80}, int64(len(want)-1)) },
		"one byte short":  func(f *os.File) { _ = f.Truncate(int64(len(want) - 1)) },
		"one byte longer": func(f *os.File) { _ = f.Truncate(int64(len(want) + 1)) },
		"emptied":         func(f *os.File) { _ = f.Truncate(0) },
	} {
		copyPath := filepath.Join(dir, "damaged.img")
		require.NoError(t, SparseCopy(copyPath, src))
		f, err := os.OpenFile(copyPath, os.O_RDWR, 0)
		require.NoError(t, err)
		damage(f)
		require.NoError(t, f.Close())
		sum, err := HashFile(copyPath)
		require.NoError(t, err)
		assert.NotEqual(t, base, sum, "%s went unnoticed", name)
	}
}
