package volume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A volume is the one thing in the store that holds data nobody can rebuild.
// These tests fail every write of every operation, damage the files afterwards
// and kill the process mid-flight, and hold the store to this: the disk of an
// existing volume is never lost, and a name is never left half-taken.

const mib = 1 << 20

func openStore(t *testing.T, root string) *Store {
	t.Helper()
	s, err := NewStore(root)
	require.NoError(t, err)
	return s
}

// writeData puts recognizable user data in a volume's disk.
func writeData(t *testing.T, v *Volume, payload string) {
	t.Helper()
	f, err := os.OpenFile(v.DiskPath, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(append([]byte("NVMTFS"), payload...), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func requireData(t *testing.T, s *Store, name, payload string) {
	t.Helper()
	v, err := s.Get(name)
	require.NoError(t, err, "volume %s is gone", name)
	data := make([]byte, 6+len(payload))
	f, err := os.Open(v.DiskPath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	_, err = f.ReadAt(data, 0)
	require.NoError(t, err)
	require.Equal(t, "NVMTFS"+payload, string(data), "the data in volume %s changed", name)
}

func names(t *testing.T, s *Store) []string {
	t.Helper()
	list, err := s.List()
	require.NoError(t, err)
	out := make([]string, 0, len(list))
	for _, v := range list {
		out = append(out, v.ID)
	}
	return out
}

func TestFaults_CreateIsAllOrNothing(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			root := t.TempDir()
			s := openStore(t, root)
			keep, err := s.Create("keep", mib)
			require.NoError(t, err)
			writeData(t, keep, "precious")

			inj := durabletest.Inject(t, f, 0, nil)
			_, err = s.Create("fresh", mib)
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			s = openStore(t, root)
			requireData(t, s, "keep", "precious")
			got := names(t, s)
			if err != nil {
				assert.Equal(t, []string{"keep"}, got, "a creation that failed left a volume behind")
				_, statErr := os.Stat(filepath.Join(root, "fresh"))
				assert.True(t, os.IsNotExist(statErr), "a failed creation still holds the name")
			} else {
				assert.ElementsMatch(t, []string{"keep", "fresh"}, got)
			}

			// The name is free to try again, and the staging left by a crash
			// is reclaimed.
			if err != nil {
				_, err = s.Create("fresh", mib)
				require.NoError(t, err)
			}
			s.SweepStaging()
			assert.Empty(t, s.Staging())
			assert.ElementsMatch(t, []string{"keep", "fresh"}, names(t, s))
		})
	}
}

func TestFaults_CreateInReadOnlyStore(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	root := t.TempDir()
	s := openStore(t, root)
	require.NoError(t, os.Chmod(root, 0o500))
	_, err := s.Create("blocked", mib)
	require.Error(t, err)
	require.NoError(t, os.Chmod(root, 0o700))
	assert.Empty(t, names(t, s))
	assert.Empty(t, s.Staging(), "a creation that could not start left staging behind")
}

// A disk that cannot be allocated (here: the size is refused) must not leave
// the name taken.
func TestFaults_CreateWhenDiskCannotBeAllocated(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	orig := allocate
	allocate = func(string, int64) error { return syscall.ENOSPC }
	_, err := s.Create("big", mib)
	allocate = orig
	require.ErrorIs(t, err, syscall.ENOSPC)
	assert.Empty(t, names(t, s))
	assert.Empty(t, s.Staging())
	_, err = s.Create("big", mib)
	require.NoError(t, err)
}

func TestFaults_SweepStagingRemovesOnlyStaging(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	v, err := s.Create("data", mib)
	require.NoError(t, err)
	writeData(t, v, "kept")

	for _, name := range []string{".create-123", ".import-456"} {
		dir := filepath.Join(root, name)
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "disk.img"), []byte("12345"), 0o600))
	}
	// Not staging: a hidden directory of something else, and a plain file.
	require.NoError(t, os.Mkdir(filepath.Join(root, ".other"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".create-file"), []byte("x"), 0o600))

	assert.Len(t, s.Staging(), 2)
	count, bytes := s.SweepStaging()
	assert.Equal(t, 2, count)
	assert.Equal(t, int64(10), bytes)
	assert.Empty(t, s.Staging())
	assert.DirExists(t, filepath.Join(root, ".other"))
	assert.FileExists(t, filepath.Join(root, ".create-file"))
	requireData(t, s, "data", "kept")
}

func TestFaults_UnreadableMetadataDoesNotCostTheData(t *testing.T) {
	damage := map[string]func(t *testing.T, meta string){
		"deleted":   func(t *testing.T, meta string) { require.NoError(t, os.Remove(meta)) },
		"empty":     func(t *testing.T, meta string) { require.NoError(t, os.WriteFile(meta, nil, 0o600)) },
		"truncated": func(t *testing.T, meta string) { require.NoError(t, os.WriteFile(meta, []byte(`{"id":"da`), 0o600)) },
		"garbage":   func(t *testing.T, meta string) { require.NoError(t, os.WriteFile(meta, []byte{0, 1, 2, 0xff}, 0o600)) },
		"null":      func(t *testing.T, meta string) { require.NoError(t, os.WriteFile(meta, []byte("null"), 0o600)) },
		"no id": func(t *testing.T, meta string) {
			require.NoError(t, os.WriteFile(meta, []byte(`{"size_bytes":5}`), 0o600))
		},
		"is a directory": func(t *testing.T, meta string) {
			require.NoError(t, os.Remove(meta))
			require.NoError(t, os.Mkdir(meta, 0o700))
		},
	}
	for name, fn := range damage {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s := openStore(t, root)
			v, err := s.Create("data", 2*mib)
			require.NoError(t, err)
			writeData(t, v, "precious")
			other, err := s.Create("other", mib)
			require.NoError(t, err)
			writeData(t, other, "also")

			fn(t, filepath.Join(root, "data", metaFile))

			s = openStore(t, root)
			assert.ElementsMatch(t, []string{"data", "other"}, names(t, s), "a volume with damaged metadata dropped out of the list")
			requireData(t, s, "data", "precious")
			requireData(t, s, "other", "also")
			got, err := s.Get("data")
			require.NoError(t, err)
			assert.Equal(t, "data", got.ID)
			assert.Equal(t, int64(2*mib), got.SizeBytes)
			assert.Equal(t, "data", got.Label)
			assert.False(t, got.CreatedAt.IsZero())

			// It can still be removed like any other volume.
			require.NoError(t, s.Remove("data"))
			assert.Equal(t, []string{"other"}, names(t, s))
		})
	}
}

func TestFaults_RebuiltMetadataIsSaved(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	_, err := s.Create("data", mib)
	require.NoError(t, err)
	meta := filepath.Join(root, "data", metaFile)
	require.NoError(t, os.WriteFile(meta, []byte("{"), 0o600))

	first, err := s.Get("data")
	require.NoError(t, err)
	saved, err := os.ReadFile(meta)
	require.NoError(t, err)
	assert.Contains(t, string(saved), `"id": "data"`)
	second, err := s.Get("data")
	require.NoError(t, err)
	assert.Equal(t, first.CreatedAt, second.CreatedAt, "the rebuilt metadata must be stable once written")
}

func TestFaults_RebuildSurvivesItsOwnWriteFailing(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			root := t.TempDir()
			s := openStore(t, root)
			v, err := s.Create("data", mib)
			require.NoError(t, err)
			writeData(t, v, "precious")
			require.NoError(t, os.WriteFile(filepath.Join(root, "data", metaFile), []byte("{"), 0o600))

			durabletest.Inject(t, f, 0, nil)
			_, err = s.Get("data")
			require.NoError(t, err, "the volume is usable even when its repaired metadata cannot be saved")
			durable.SetFault(nil)
			requireData(t, openStore(t, root), "data", "precious")
		})
	}
}

func TestFaults_VolumeWithoutDiskIsNotAVolume(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	_, err := s.Create("data", mib)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "data", "disk.img")))
	require.NoError(t, os.Remove(filepath.Join(root, "data", metaFile)))

	_, err = s.Get("data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Empty(t, names(t, s))
}

func TestFaults_ImportIsAllOrNothing(t *testing.T) {
	payload := "NVMTFS" + strings.Repeat("imported ", 1000)
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			root := t.TempDir()
			s := openStore(t, root)
			inj := durabletest.Inject(t, f, 0, nil)
			_, err := s.Import(context.Background(), "imported", "imported", int64(len(payload)), strings.NewReader(payload))
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			s = openStore(t, root)
			if err != nil {
				assert.Empty(t, names(t, s))
				_, err = s.Import(context.Background(), "imported", "imported", int64(len(payload)), strings.NewReader(payload))
				require.NoError(t, err, "the name must be free after a failed import")
			}
			v, err := s.Get("imported")
			require.NoError(t, err)
			data, err := os.ReadFile(v.DiskPath)
			require.NoError(t, err)
			assert.Equal(t, payload, string(data))
		})
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestFaults_ImportStreamBreaksHalfWay(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	broken := errors.New("connection reset")
	_, err := s.Import(context.Background(), "imported", "imported", 4*mib, &failingReader{data: make([]byte, mib), err: broken})
	require.ErrorIs(t, err, broken)
	assert.Empty(t, names(t, s))
	assert.Empty(t, s.Staging(), "a failed import cleans its staging up")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestFaults_ImportCanceled(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Import(ctx, "imported", "imported", mib, strings.NewReader(strings.Repeat("x", mib)))
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, names(t, s))
	assert.Empty(t, s.Staging())
}

func TestFaults_RemoveThenCreateAgain(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	for i := range 5 {
		v, err := s.Create("cycle", mib)
		require.NoError(t, err, "round %d", i)
		writeData(t, v, fmt.Sprint(i))
		require.NoError(t, s.Remove("cycle"))
	}
	assert.Empty(t, names(t, s))
}
