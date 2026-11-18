package pkg

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	// Digests are remembered only for files left alone for settleTime.
	time.Sleep(settleTime + 50*time.Millisecond)
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

// A file written a moment ago may be written again with the same size and get
// the same times, so its digest is not remembered: the next call reads it.
func TestFileDigestIsNotRememberedForAFileJustWritten(t *testing.T) {
	resetDigestCache()
	p := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(p, []byte("one"), 0o644))
	_, err := fileDigest(p)
	require.NoError(t, err)
	id, err := statID(p)
	require.NoError(t, err)
	_, ok := digestCache.Load(digestKey{p, id})
	require.False(t, ok, "a digest was remembered %v after the file was written", settleTime)
	require.NoError(t, SaveDigests(filepath.Dir(p), []File{{HostPath: p}}, ""))
	require.Empty(t, readDigestIndex(filepath.Dir(p)).Files, "nor is it saved for other processes")

	time.Sleep(settleTime + 50*time.Millisecond)
	_, err = fileDigest(p)
	require.NoError(t, err)
	_, ok = digestCache.Load(digestKey{p, id})
	require.True(t, ok, "a file left alone is remembered")
}

func TestFileIDSettled(t *testing.T) {
	now := time.Unix(1000, 0)
	at := func(d time.Duration) int64 { return now.Add(d).UnixNano() }
	require.True(t, fileID{ModNs: at(-time.Hour), ChangeNs: at(-settleTime)}.settled(now))
	require.False(t, fileID{ModNs: at(-time.Hour), ChangeNs: at(-settleTime + 1)}.settled(now), "the change time decides")
	require.True(t, fileID{ModNs: at(time.Minute), ChangeNs: at(-time.Hour)}.settled(now),
		"a modification time set in the future does not keep a file from being remembered")
	require.True(t, fileID{ModNs: at(-settleTime)}.settled(now), "without a change time the modification time decides")
	require.False(t, fileID{ModNs: at(-settleTime + 1)}.settled(now))
	require.False(t, fileID{ModNs: at(time.Minute)}.settled(now), "a time in the future is not settled")
}
