package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return Prefix + hex.EncodeToString(s[:])
}

func TestPutFileAndBytes(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	src := filepath.Join(t.TempDir(), "runtime")
	data := []byte("a runtime binary")
	require.NoError(t, os.WriteFile(src, data, 0o755))

	d, err := s.PutFile(src)
	require.NoError(t, err)
	require.Equal(t, digestOf(data), d)
	require.True(t, s.Has(d))
	got, err := s.ReadBytes(d)
	require.NoError(t, err)
	require.Equal(t, data, got)
	require.NoError(t, s.Verify(d))

	p, err := s.Path(d)
	require.NoError(t, err)
	info, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o444), info.Mode().Perm())

	// The copy is independent of its source.
	require.NoError(t, os.WriteFile(src, []byte("changed"), 0o755))
	got, err = s.ReadBytes(d)
	require.NoError(t, err)
	require.Equal(t, data, got)

	// Adding the same content again is a no-op.
	d2, err := s.PutBytes(data)
	require.NoError(t, err)
	require.Equal(t, d, d2)
	entries, err := os.ReadDir(s.Root())
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestCorruptionIsDetected(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	d, err := s.PutBytes([]byte("recipe"))
	require.NoError(t, err)
	p, _ := s.Path(d)
	require.NoError(t, os.Chmod(p, 0o644))
	require.NoError(t, os.WriteFile(p, []byte("tampered"), 0o644))
	_, err = s.ReadBytes(d)
	require.ErrorContains(t, err, "corrupt")
	require.ErrorContains(t, s.Verify(d), "corrupt")

	_, err = s.ReadBytes(digestOf([]byte("absent")))
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.Path("sha256:../../etc/passwd")
	require.Error(t, err)
}

func TestPruneKeepsReferencedAndRecent(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	keep, _ := s.PutBytes([]byte("kept"))
	old, _ := s.PutBytes([]byte("old"))
	recent, _ := s.PutBytes([]byte("recent"))
	past := time.Now().Add(-2 * time.Hour)
	for _, d := range []string{keep, old} {
		p, _ := s.Path(d)
		require.NoError(t, os.Chtimes(p, past, past))
	}

	removed, err := s.Prune(func(d string) bool { return d == keep }, time.Hour)
	require.NoError(t, err)
	require.Equal(t, []string{old}, removed)
	require.True(t, s.Has(keep))
	require.True(t, s.Has(recent))
	require.False(t, s.Has(old))

	// Reusing an old blob restarts its grace period.
	again, _ := s.PutBytes([]byte("old"))
	require.Equal(t, old, again)
	p, _ := s.Path(keep)
	require.NoError(t, os.Chtimes(p, past, past))
	removed, err = s.Prune(func(string) bool { return false }, time.Hour)
	require.NoError(t, err)
	require.Equal(t, []string{keep}, removed)
}
