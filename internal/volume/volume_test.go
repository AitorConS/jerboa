package volume

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStore_Create_rejects_traversal(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(filepath.Join(root, "store"))
	require.NoError(t, err)

	sentinel := filepath.Join(root, "escape")
	_, err = store.Create("../escape", 1<<20)
	require.Error(t, err)
	_, statErr := os.Stat(sentinel)
	require.True(t, os.IsNotExist(statErr), "traversal name created a file outside the store")

	require.Error(t, store.Remove("../escape"))
	require.Error(t, store.Remove("a/b"))
	_, err = store.Get("../escape")
	require.Error(t, err)
}

func TestStore_CreateAndGet(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	require.NoError(t, err)

	v, err := store.Create("data", 1<<20) // 1 MiB
	require.NoError(t, err)
	require.Equal(t, "data", v.ID)
	require.Equal(t, int64(1<<20), v.SizeBytes)
	require.FileExists(t, v.DiskPath)

	got, err := store.Get("data")
	require.NoError(t, err)
	require.Equal(t, "data", got.ID)
}

func TestStore_Create_duplicate(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	_, err = store.Create("dup", 0)
	require.NoError(t, err)
	_, err = store.Create("dup", 0)
	require.Error(t, err)
}

func TestStore_Create_default_size(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	v, err := store.Create("default", 0)
	require.NoError(t, err)
	require.Equal(t, int64(defaultSizeBytes), v.SizeBytes)
}

func TestStore_Get_notfound(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	_, err = store.Get("missing")
	require.Error(t, err)
}

func TestStore_List(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	_, err = store.Create("vol1", 1<<20)
	require.NoError(t, err)
	_, err = store.Create("vol2", 1<<20)
	require.NoError(t, err)

	vols, err := store.List()
	require.NoError(t, err)
	require.Len(t, vols, 2)
}

func TestStore_Remove(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	v, err := store.Create("todel", 1<<20)
	require.NoError(t, err)
	diskPath := v.DiskPath

	require.NoError(t, store.Remove("todel"))

	_, err = store.Get("todel")
	require.Error(t, err)
	require.NoFileExists(t, diskPath)
}

func TestStore_Remove_notfound(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	require.Error(t, store.Remove("ghost"))
}

func TestAllocateDisk_creates_file_of_correct_size(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "disk*.img")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	const sz = 4096
	require.NoError(t, allocateDisk(f.Name(), sz))

	info, err := os.Stat(f.Name())
	require.NoError(t, err)
	require.Equal(t, int64(sz), info.Size())
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		input string
		want  int64
		isErr bool
	}{
		{"1G", 1 << 30, false},
		{"512M", 512 << 20, false},
		{"2048K", 2048 << 10, false},
		{"1073741824", 1073741824, false},
		{"", 0, false},
		{"bad", 0, true},
		{"1X", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseSize(tc.input)
			if tc.isErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// A removal is a rename followed by a delete: a daemon killed between the
// two leaves a staging directory the sweep removes, never a volume that is
// still listed without its disk.
func TestRemoveGoesThroughStaging(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	_, err = s.Create("data", 4096)
	require.NoError(t, err)

	// What the rename leaves if the delete never runs.
	staged := filepath.Join(root, ".remove-data-1")
	require.NoError(t, os.Rename(filepath.Join(root, "data"), staged))
	vols, err := s.List()
	require.NoError(t, err)
	require.Empty(t, vols, "a volume being removed must not be listed")
	require.Equal(t, []string{staged}, s.Staging())
	count, _ := s.SweepStaging(0)
	require.Equal(t, 1, count)
	require.NoDirExists(t, staged)

	// And a whole removal leaves nothing behind.
	_, err = s.Create("data", 4096)
	require.NoError(t, err)
	require.NoError(t, s.Remove("data"))
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}
