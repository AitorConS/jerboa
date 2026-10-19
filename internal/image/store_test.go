package image

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMarshal_Error(t *testing.T) {
	_, err := Marshal(Manifest{})
	require.NoError(t, err)
}

func TestParse_shortDiskDigest(t *testing.T) {
	m := validManifest()
	m.DiskDigest = "sha256:"
	data, err := Marshal(m)
	require.NoError(t, err)
	_, err = Parse(data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "diskDigest must start with sha256")
}

func TestParse_diskDigestExactlySha256Prefix(t *testing.T) {
	m := validManifest()
	m.DiskDigest = "sha256:"
	data, err := Marshal(m)
	require.NoError(t, err)
	_, err = Parse(data)
	require.Error(t, err)
}

func TestParse_validWithEnv(t *testing.T) {
	m := validManifest()
	m.Config.Env = []string{"FOO=bar", "BAZ=qux"}
	data, err := Marshal(m)
	require.NoError(t, err)
	got, err := Parse(data)
	require.NoError(t, err)
	require.Equal(t, []string{"FOO=bar", "BAZ=qux"}, got.Config.Env)
}

func TestDigestSHA256_empty(t *testing.T) {
	d := DigestSHA256([]byte{})
	require.Equal(t, "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", d)
}

func TestDigestSHA256_knownValue(t *testing.T) {
	d := DigestSHA256([]byte("hello"))
	require.Equal(t, "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", d)
}

func makeDiskFile(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "disk-*.img")
	require.NoError(t, err)
	_, err = f.WriteString("fake disk image content")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func makeStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "images"))
	require.NoError(t, err)
	return s
}

func TestStore_Put_Get(t *testing.T) {
	s := makeStore(t)
	m := validManifest()
	disk := makeDiskFile(t)

	require.NoError(t, s.Put("hello", "latest", m, disk))

	got, diskPath, err := s.Get("hello:latest")
	require.NoError(t, err)
	require.Equal(t, "hello", got.Name)
	require.Equal(t, "latest", got.Tag)
	require.FileExists(t, diskPath)
}

func TestStore_Get_by_sha256(t *testing.T) {
	s := makeStore(t)
	m := validManifest()
	disk := makeDiskFile(t)
	require.NoError(t, s.Put("hello", "latest", m, disk))

	got, _, err := s.Get("hello:latest")
	require.NoError(t, err)

	_, _, err = s.Get(got.DiskDigest)
	require.NoError(t, err)
}

func TestStore_Get_by_sha256_prefix(t *testing.T) {
	s := makeStore(t)
	m := validManifest()
	disk := makeDiskFile(t)
	require.NoError(t, s.Put("hello", "latest", m, disk))

	got, _, err := s.Get("hello:latest")
	require.NoError(t, err)

	prefix := got.DiskDigest[:20]
	_, _, err = s.Get(prefix)
	require.NoError(t, err)
}

func TestStore_Get_not_found(t *testing.T) {
	s := makeStore(t)
	_, _, err := s.Get("nonexistent:latest")
	require.Error(t, err)
}

func TestStore_List(t *testing.T) {
	s := makeStore(t)

	disk1 := makeDiskFile(t)
	disk2 := makeDiskFile(t)
	require.NoError(t, os.WriteFile(disk2, []byte("different content"), 0o644))

	require.NoError(t, s.Put("a", "latest", validManifest(), disk1))
	m2 := validManifest()
	m2.Name = "b"
	require.NoError(t, s.Put("b", "latest", m2, disk2))

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestStore_List_empty(t *testing.T) {
	s := makeStore(t)
	list, err := s.List()
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestStore_Remove(t *testing.T) {
	s := makeStore(t)
	disk := makeDiskFile(t)
	require.NoError(t, s.Put("hello", "latest", validManifest(), disk))

	require.NoError(t, s.Remove("hello:latest"))

	list, err := s.List()
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestStore_Remove_not_found(t *testing.T) {
	s := makeStore(t)
	require.Error(t, s.Remove("nonexistent:latest"))
}

func TestStore_Remove_bySHAPrefix(t *testing.T) {
	s := makeStore(t)
	disk := makeDiskFile(t)
	require.NoError(t, s.Put("hello", "latest", validManifest(), disk))

	got, _, err := s.Get("hello:latest")
	require.NoError(t, err)

	require.NoError(t, s.Remove("hello:latest"))
	_, _, err = s.Get(got.DiskDigest)
	require.Error(t, err)
}

func TestStore_Remove_removesImageDir(t *testing.T) {
	s := makeStore(t)
	disk := makeDiskFile(t)
	require.NoError(t, s.Put("hello", "latest", validManifest(), disk))

	require.NoError(t, s.Remove("hello:latest"))

	_, _, err := s.Get("hello:latest")
	require.Error(t, err)
}

func TestStore_Remove_keepsImageDirIfOtherRefs(t *testing.T) {
	s := makeStore(t)
	disk := makeDiskFile(t)
	m := validManifest()

	require.NoError(t, s.Put("hello", "latest", m, disk))
	require.NoError(t, s.Put("hello", "v1", m, disk))

	require.NoError(t, s.Remove("hello:latest"))

	_, _, err := s.Get("hello:v1")
	require.NoError(t, err)

	_, _, err = s.Get("hello:latest")
	require.Error(t, err)
}

func TestStore_Multiple_tags_same_image(t *testing.T) {
	s := makeStore(t)
	disk := makeDiskFile(t)
	m := validManifest()

	require.NoError(t, s.Put("hello", "latest", m, disk))
	require.NoError(t, s.Put("hello", "v1.0", m, disk))

	// Each tag is listed with its own manifest, sharing one disk.
	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "latest", list[0].Tag)
	require.Equal(t, "v1.0", list[1].Tag)
	require.Equal(t, list[0].DiskDigest, list[1].DiskDigest)

	require.NoError(t, s.Remove("hello:v1.0"))
	_, _, err = s.Get("hello:latest")
	require.NoError(t, err)
}

func TestStore_DiskPath(t *testing.T) {
	s := makeStore(t)
	disk := makeDiskFile(t)
	require.NoError(t, s.Put("hello", "latest", validManifest(), disk))

	p, err := s.DiskPath("hello:latest")
	require.NoError(t, err)
	require.FileExists(t, p)
	require.Contains(t, p, "disk.img")
}

func TestStore_DiskPath_not_found(t *testing.T) {
	s := makeStore(t)
	_, err := s.DiskPath("nonexistent:latest")
	require.Error(t, err)
}

func TestStore_Put_missing_disk(t *testing.T) {
	s := makeStore(t)
	m := validManifest()
	err := s.Put("hello", "latest", m, "/nonexistent/disk.img")
	require.Error(t, err)
}

func TestStore_Put_manifest_parse_error(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "images"))
	require.NoError(t, err)

	imgDir := filepath.Join(dir, "images", "abc123")
	require.NoError(t, os.MkdirAll(imgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(imgDir, "manifest.json"), []byte("bad json"), 0o644))

	refs := map[string]string{"hello:latest": "abc123"}
	data, err := json.MarshalIndent(refs, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "images", "refs.json"), data, 0o644))

	_, _, err = s.Get("hello:latest")
	require.Error(t, err)
}

func TestStore_Get_missing_manifest(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "images"))
	require.NoError(t, err)

	refs := map[string]string{"hello:latest": "nonexistentsha"}
	data, err := json.MarshalIndent(refs, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "images", "refs.json"), data, 0o644))

	_, _, err = s.Get("hello:latest")
	require.Error(t, err)
}

func TestNewStore_mkdir_error(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "afile")
	require.NoError(t, os.WriteFile(filePath, []byte("x"), 0o644))
	_, err := NewStore(filePath)
	require.Error(t, err)
}

func TestStore_readRefs_parse_error(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "images")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "refs.json"), []byte("{bad"), 0o644))

	s := &Store{root: root}
	_, err := s.List()
	require.Error(t, err)
}

func TestStore_writeRefs_error(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "images")
	require.NoError(t, os.MkdirAll(root, 0o755))

	filePath := filepath.Join(root, "refs.json")
	require.NoError(t, os.WriteFile(filePath, []byte("{}"), 0o644))
	os.Chmod(filePath, 0o444)
	defer os.Chmod(filePath, 0o755)

	s := &Store{root: root}
	err := s.addRef("hello:latest", "abc123")
	require.Error(t, err)
}

func TestFileSHA256(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "file")
		require.NoError(t, os.WriteFile(p, []byte("hello"), 0o644))
		d, err := fileSHA256(p)
		require.NoError(t, err)
		require.Equal(t, "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", d)
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := fileSHA256("/nonexistent/file")
		require.Error(t, err)
	})
}

func TestCopyFile(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		dst := filepath.Join(dir, "dst")
		require.NoError(t, os.WriteFile(src, []byte("content"), 0o644))
		require.NoError(t, copyFile(src, dst))
		data, err := os.ReadFile(dst)
		require.NoError(t, err)
		require.Equal(t, "content", string(data))
	})

	t.Run("missing src", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "dst")
		err := copyFile("/nonexistent/src", dst)
		require.Error(t, err)
	})

	t.Run("dst dir missing", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))
		err := copyFile(src, filepath.Join("/nonexistent/dir", "dst"))
		require.Error(t, err)
	})
}

func TestStripPrefix(t *testing.T) {
	require.Equal(t, "abc123", stripPrefix("sha256:abc123"))
	require.Equal(t, "noprefix", stripPrefix("noprefix"))
}

func imageDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

func TestStore_PruneUnreferenced(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	s, err := NewStore(root)
	require.NoError(t, err)
	write := func(content string) string {
		p := filepath.Join(t.TempDir(), "disk.img")
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
		return p
	}
	first, err := s.PutMove("web", "latest", validManifest(), write("build one"))
	require.NoError(t, err)
	second, err := s.PutMove("web", "latest", validManifest(), write("build two"))
	require.NoError(t, err)
	require.NotEqual(t, first.DiskDigest, second.DiskDigest)
	// A directory that is not an image is never touched.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "not-an-image"), 0o755))
	require.Len(t, imageDirs(t, root), 3, "the rebuild left the first image behind")

	// An image a VM still uses survives.
	removed, err := s.PruneUnreferenced(func(d string) bool { return d == first.DiskDigest })
	require.NoError(t, err)
	require.Empty(t, removed)

	removed, err = s.PruneUnreferenced(nil)
	require.NoError(t, err)
	require.Equal(t, []string{first.DiskDigest}, removed)
	require.ElementsMatch(t, []string{stripPrefix(second.DiskDigest), "not-an-image"}, imageDirs(t, root))
	_, _, err = s.Get("web:latest")
	require.NoError(t, err)
}

// Tags that share a disk keep their own metadata: a later build with other
// defaults must not change what an earlier tag reports.
func TestStore_TagsSharingDiskKeepOwnManifest(t *testing.T) {
	s := makeStore(t)
	write := func() string {
		p := filepath.Join(t.TempDir(), "disk.img")
		require.NoError(t, os.WriteFile(p, []byte("same reproducible disk"), 0o600))
		return p
	}
	small := validManifest()
	small.Config.Memory = "128M"
	big := validManifest()
	big.Config.Memory = "1G"
	a, err := s.PutMove("api", "v1", small, write())
	require.NoError(t, err)
	b, err := s.PutMove("worker", "v1", big, write())
	require.NoError(t, err)
	require.Equal(t, a.DiskDigest, b.DiskDigest)

	m, _, err := s.Resolve("api:v1")
	require.NoError(t, err)
	require.Equal(t, "api", m.Name)
	require.Equal(t, "128M", m.Config.Memory)
	m, _, err = s.Resolve("worker:v1")
	require.NoError(t, err)
	require.Equal(t, "worker", m.Name)
	require.Equal(t, "1G", m.Config.Memory)

	// Removing one tag keeps the shared disk for the other.
	require.NoError(t, s.Remove("api:v1"))
	_, _, err = s.Get("worker:v1")
	require.NoError(t, err)
}

// Images stored before per-tag manifests existed still resolve.
func TestStore_ResolvesLegacyImageWithoutTagManifest(t *testing.T) {
	s := makeStore(t)
	m, err := s.PutMove("old", "latest", validManifest(), makeDiskFile(t))
	require.NoError(t, err)
	dir := filepath.Join(s.root, stripPrefix(m.DiskDigest))
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "tags")))
	got, _, err := s.Get("old:latest")
	require.NoError(t, err)
	require.Equal(t, m.DiskDigest, got.DiskDigest)
	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)

	// A legacy tag reports its own name even when the shared manifest was
	// last written for another tag.
	other := validManifest()
	other.Name, other.Tag, other.DiskDigest = "newer", "v2", m.DiskDigest
	data, err := Marshal(other)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644))
	got, _, err = s.Get("old:latest")
	require.NoError(t, err)
	require.Equal(t, "old", got.Name)
	require.Equal(t, "latest", got.Tag)
}

func TestStore_TrustedSkipsRehashUntilDiskChanges(t *testing.T) {
	s := makeStore(t)
	disk := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(disk, []byte("original contents"), 0o600))
	m, err := s.PutMove("app", "v1", validManifest(), disk)
	require.NoError(t, err)
	stored := filepath.Join(s.root, stripPrefix(m.DiskDigest), "disk.img")

	info, err := os.Stat(stored)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o444), info.Mode().Perm(), "stored disks are read-only")
	require.FileExists(t, filepath.Join(filepath.Dir(stored), "verified.json"))
	require.True(t, s.Trusted(stored, m.DiskDigest))

	// Only the store's own disk for that digest is ever trusted.
	require.False(t, s.Trusted(disk, m.DiskDigest))
	require.False(t, s.Trusted(stored, "sha256:"+strings.Repeat("0", 64)))

	// Same size, mtime restored: the status-change time still gives it away.
	require.NoError(t, os.Chmod(stored, 0o644))
	require.NoError(t, os.WriteFile(stored, []byte("tampered contents"), 0o644))
	require.NoError(t, os.Chtimes(stored, info.ModTime(), info.ModTime()))
	require.False(t, s.Trusted(stored, m.DiskDigest))

	// Putting the original bytes back is accepted again after a re-hash.
	require.NoError(t, os.WriteFile(stored, []byte("original contents"), 0o644))
	require.True(t, s.Trusted(stored, m.DiskDigest))
}

func TestStoreTagAddsNameForSameDisk(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.Put("web", "local", validManifest(), makeDiskFile(t)))
	require.NoError(t, s.SetRecipe("web:local", "sha256:"+strings.Repeat("c", 64)))
	m, err := s.Tag("web:local", "alice/web", "v1")
	require.NoError(t, err)
	require.Equal(t, "alice/web", m.Name)
	got, disk, err := s.Get("alice/web:v1")
	require.NoError(t, err)
	require.Equal(t, "v1", got.Tag)
	require.Equal(t, "sha256:"+strings.Repeat("c", 64), got.Recipe)
	_, orig, err := s.Get("web:local")
	require.NoError(t, err)
	require.Equal(t, orig, disk)
}
