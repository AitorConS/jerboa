package builder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCacheKeyBasic(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644))

	key, err := CacheKey(dir, LangGo, "", nil)
	require.NoError(t, err)
	require.NotEmpty(t, key)
	require.Len(t, key, 16)
}

func TestCacheKeyDeterministic(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))

	key1, err := CacheKey(dir, LangGo, "", nil)
	require.NoError(t, err)
	key2, err := CacheKey(dir, LangGo, "", nil)
	require.NoError(t, err)
	require.Equal(t, key1, key2)
}

func TestCacheKeyChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))

	key1, err := CacheKey(dir, LangGo, "", nil)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644))
	key2, err := CacheKey(dir, LangGo, "", nil)
	require.NoError(t, err)

	require.NotEqual(t, key1, key2)
}

func TestCacheKeyChangesWithLang(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))

	key1, err := CacheKey(dir, LangGo, "", nil)
	require.NoError(t, err)
	key2, err := CacheKey(dir, LangNode, "", nil)
	require.NoError(t, err)
	require.NotEqual(t, key1, key2)
}

func TestCacheKeyChangesWithEntrypoint(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))

	key1, err := CacheKey(dir, LangGo, "", nil)
	require.NoError(t, err)
	key2, err := CacheKey(dir, LangGo, "cmd/server", nil)
	require.NoError(t, err)
	require.NotEqual(t, key1, key2)
}

func TestCacheKeyIgnoresIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))

	key1, err := CacheKey(dir, LangGo, "", nil)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("changed\n"), 0o644))

	key2, err := CacheKey(dir, LangGo, "", nil)
	require.NoError(t, err)
	require.Equal(t, key1, key2, "cache key should not change when .git files change")
}

func TestBuildCacheStoreAndGet(t *testing.T) {
	cacheDir := t.TempDir()
	bc, err := NewBuildCache(cacheDir)
	require.NoError(t, err)

	entry := CacheEntry{
		Key:         "abc123",
		ImageDigest: "sha256:def456",
		SourceDir:   "/tmp/myapp",
		Lang:        "go",
	}
	require.NoError(t, bc.Store(entry))

	got, err := bc.Get("abc123")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, entry.Key, got.Key)
	require.Equal(t, entry.ImageDigest, got.ImageDigest)
	require.Equal(t, entry.SourceDir, got.SourceDir)
	require.Equal(t, entry.Lang, got.Lang)
}

func TestBuildCacheGetNotFound(t *testing.T) {
	cacheDir := t.TempDir()
	bc, err := NewBuildCache(cacheDir)
	require.NoError(t, err)

	got, err := bc.Get("nonexistent")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestBuildCacheHas(t *testing.T) {
	cacheDir := t.TempDir()
	bc, err := NewBuildCache(cacheDir)
	require.NoError(t, err)

	require.False(t, bc.Has("abc123"))

	require.NoError(t, bc.Store(CacheEntry{Key: "abc123"}))
	require.True(t, bc.Has("abc123"))
}
