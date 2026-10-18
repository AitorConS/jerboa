package pkg

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func resetDigestCache() {
	digestCache.Range(func(k, _ any) bool { digestCache.Delete(k); return true })
}

func TestFileDigestFollowsFileChanges(t *testing.T) {
	resetDigestCache()
	p := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(p, []byte("one"), 0o644))
	d1, err := fileDigest(p)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256([]byte("one")), d1)

	require.NoError(t, os.WriteFile(p, []byte("two"), 0o644))
	d2, err := fileDigest(p)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256([]byte("two")), d2, "a changed file is hashed again")
}

func TestSaveAndSeedDigests(t *testing.T) {
	resetDigestCache()
	dir := t.TempDir()
	a := filepath.Join(dir, "files", "bin", "app")
	require.NoError(t, os.MkdirAll(filepath.Dir(a), 0o755))
	require.NoError(t, os.WriteFile(a, []byte("binary"), 0o755))
	outside := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o644))
	files := []File{{HostPath: a, GuestPath: "bin/app"}, {HostPath: outside}, {GuestPath: "data", IsDir: true}}
	for _, f := range files[:2] {
		_, err := fileDigest(f.HostPath)
		require.NoError(t, err)
	}
	require.NoError(t, SaveDigests(dir, files, "abc"))
	idx := readDigestIndex(dir)
	require.Len(t, idx.Files, 1, "only files under the package directory are recorded")
	require.Contains(t, idx.Files, "files/bin/app")
	require.True(t, archiveVerified(dir, "abc", files[:1]))
	require.False(t, archiveVerified(dir, "other", files[:1]))

	// A new process starts with an empty cache and reloads the saved digest.
	resetDigestCache()
	SeedDigests(dir)
	id, err := statID(a)
	require.NoError(t, err)
	_, ok := digestCache.Load(digestKey{a, id})
	require.True(t, ok)

	// Any change to the file invalidates both the digest and the verification.
	require.NoError(t, os.WriteFile(a, []byte("binarY"), 0o755))
	resetDigestCache()
	SeedDigests(dir)
	id, err = statID(a)
	require.NoError(t, err)
	_, ok = digestCache.Load(digestKey{a, id})
	require.False(t, ok)
	require.False(t, archiveVerified(dir, "abc", files[:1]))
}
