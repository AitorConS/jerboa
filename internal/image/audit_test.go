package image

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditTamperedDisk(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.Put("test", "latest", validManifest(), makeDiskFile(t)))
	_, disk, err := s.Get("test")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(disk, []byte("tampered"), 0o644))
	_, _, err = s.Get("test")
	require.ErrorContains(t, err, "integrity mismatch")
}
func TestAuditRemoveDigestRemovesAliases(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	disk := makeDiskFile(t)
	require.NoError(t, s.Put("a", "latest", validManifest(), disk))
	require.NoError(t, s.Put("b", "latest", validManifest(), disk))
	m, _, err := s.Get("a")
	require.NoError(t, err)
	require.NoError(t, s.Remove(m.DiskDigest))
	all, err := s.List()
	require.NoError(t, err)
	require.Empty(t, all)
	require.Error(t, s.Remove(m.DiskDigest))
}

func TestResolveRejectsManifestDigestMismatch(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.Put("test", "latest", validManifest(), makeDiskFile(t)))
	m, disk, err := s.Resolve("test")
	require.NoError(t, err)
	m.DiskDigest = "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"
	data, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(disk), "manifest.json"), data, 0o644))
	_, _, err = s.Resolve("test")
	require.ErrorContains(t, err, "integrity mismatch")
}
