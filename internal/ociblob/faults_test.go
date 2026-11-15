package ociblob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about a blob store whose writes are cut short and whose
// directory holds what a crash leaves. A name in the store is a digest; the
// store must never give that name to bytes that were not all read.

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func dirNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// brokenReader yields some bytes and then fails, as a download that drops or
// a source disk that errors does.
type brokenReader struct {
	data []byte
	err  error
}

func (b *brokenReader) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

// A source that fails part of the way stores nothing: not under the digest
// of the whole blob, not under the digest of the part that arrived, and not
// as a temporary file.
func TestFaults_PutFromAFailingSourceStoresNothing(t *testing.T) {
	content := bytes.Repeat([]byte("layer bytes "), 4000)
	for name, cause := range map[string]error{
		"connection reset": syscall.ECONNRESET,
		"io error":         syscall.EIO,
		"unexpected EOF":   io.ErrUnexpectedEOF,
	} {
		for _, arrived := range []int{0, 1, len(content) / 2, len(content) - 1} {
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				s, err := NewStore(root)
				require.NoError(t, err)
				digest, size, err := s.Put(&brokenReader{data: content[:arrived], err: cause})
				require.ErrorIs(t, err, cause)
				assert.Empty(t, digest)
				assert.Zero(t, size)
				assert.False(t, s.Exists(digestOf(content)))
				assert.False(t, s.Exists(digestOf(content[:arrived])), "the part that arrived was stored as a blob")
				assert.Empty(t, dirNames(t, root), "a failed put left a file behind")
				list, err := s.List()
				require.NoError(t, err)
				assert.Empty(t, list)
			})
		}
	}
}

// A store whose directory refuses new files (a read-only remount, a full
// disk at create) fails the put and still serves what it holds.
func TestFaults_PutIntoAStoreThatCannotBeWritten(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX directory permissions and a non-root user")
	}
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	held, _, err := s.Put(strings.NewReader("held"))
	require.NoError(t, err)
	require.NoError(t, os.Chmod(root, 0o555))
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	_, _, err = s.Put(strings.NewReader("new"))
	require.Error(t, err)
	require.Error(t, s.Delete(held), "a delete that did not happen reported success")
	rc, err := s.Open(held)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, rc.Close())
	require.NoError(t, err)
	assert.Equal(t, "held", string(data))
}

// What a killed process leaves in the directory (temporary files of puts in
// flight) and what does not belong there are not blobs: they are not listed,
// not opened, and do not disturb the blobs beside them.
func TestFaults_LeftoversAreNotBlobs(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	digest, _, err := s.Put(strings.NewReader("a blob"))
	require.NoError(t, err)
	for _, name := range []string{"blob-123456789", "blob-", ".DS_Store", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("a", 64) + ".tmp"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("half a blob"), 0o600))
	}
	require.NoError(t, os.Mkdir(filepath.Join(root, strings.Repeat("d", 64)), 0o755))

	list, err := s.List()
	require.NoError(t, err)
	assert.Equal(t, []string{digest}, list)
	_, err = s.Open("sha256:" + strings.Repeat("A", 64))
	require.Error(t, err)
	// Storing the same content again while leftovers are around works.
	again, _, err := s.Put(strings.NewReader("a blob"))
	require.NoError(t, err)
	assert.Equal(t, digest, again)
}

func TestFaults_ListOfAMissingStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	s, err := NewStore(root)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(root))
	_, err = s.List()
	require.Error(t, err, "a store whose directory is gone listed as empty")
	_, _, err = s.Put(strings.NewReader("x"))
	require.Error(t, err)
	require.NoError(t, s.Delete(digestOf([]byte("x"))), "deleting what is not there is not an error")

	_, err = NewStore(filepath.Join(os.DevNull, "blobs"))
	require.Error(t, err)
}

// Many writers store the same blob and different blobs at once while readers
// list and open: every put succeeds, every blob read back is whole.
func TestFaults_ConcurrentPutsAndReads(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	shared := bytes.Repeat([]byte("shared layer "), 5000)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 16 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			content := shared
			if i%2 == 1 {
				content = bytes.Repeat([]byte{byte(i)}, 4096)
			}
			d, size, err := s.Put(bytes.NewReader(content))
			switch {
			case err != nil:
				errs <- err
			case d != digestOf(content) || size != int64(len(content)):
				errs <- errors.New("a put returned another blob's digest or size")
			}
		}()
		go func() {
			defer wg.Done()
			list, err := s.List()
			if err != nil {
				errs <- err
				return
			}
			for _, d := range list {
				rc, err := s.Open(d)
				if err != nil {
					errs <- err
					continue
				}
				data, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil || digestOf(data) != d {
					errs <- errors.New("a listed blob did not read back as its digest: " + d)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	list, err := s.List()
	require.NoError(t, err)
	assert.Len(t, list, 9)
	assert.Len(t, dirNames(t, root), 9, "temporary files were left behind")
}

// A blob damaged on disk after it was stored.
func TestFaults_DamagedBlob(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	content := []byte("a layer that will be damaged")
	digest, _, err := s.Put(bytes.NewReader(content))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, strings.TrimPrefix(digest, "sha256:")), content[:5], 0o644))

	rc, err := s.Open(digest)
	require.NoError(t, err)
	data, rerr := io.ReadAll(rc)
	require.NoError(t, rc.Close())
	require.ErrorIs(t, rerr, ErrCorrupt, "the damaged blob was read without an error: %q", data)
	_, _, err = s.Put(bytes.NewReader(content))
	require.NoError(t, err)
	rc, err = s.Open(digest)
	require.NoError(t, err)
	data, err = io.ReadAll(rc)
	require.NoError(t, rc.Close())
	require.NoError(t, err)
	assert.Equal(t, content, data, "storing the blob again did not repair it")
}

func FuzzDigestPath(f *testing.F) {
	for _, seed := range []string{
		"sha256:" + strings.Repeat("a", 64), strings.Repeat("a", 64), "sha256:" + strings.Repeat("A", 64),
		"sha256:../../../../etc/passwd", "sha256:" + strings.Repeat("a", 60) + "/../x", "sha256:sha256:" + strings.Repeat("a", 64),
		"", "sha256:", "sha256:" + strings.Repeat("a", 63) + "\x00",
	} {
		f.Add(seed)
	}
	root := f.TempDir()
	s, err := NewStore(root)
	require.NoError(f, err)
	outside := filepath.Join(filepath.Dir(root), "outside")
	f.Fuzz(func(t *testing.T, digest string) {
		p, err := s.pathForDigest(digest)
		if err != nil {
			require.False(t, s.Exists(digest))
			require.Error(t, s.Delete(digest), "a malformed digest was deleted")
			_, oerr := s.Open(digest)
			require.Error(t, oerr)
			return
		}
		require.Equal(t, root, filepath.Dir(p), "digest %q names a path outside the store", digest)
		require.Len(t, filepath.Base(p), 64)
		require.NoFileExists(t, outside)
	})
}
