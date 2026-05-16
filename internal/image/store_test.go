package image

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)

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
