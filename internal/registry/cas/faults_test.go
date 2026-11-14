package cas

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about what the blob store does when a write is interrupted,
// the disk fills up, the daemon is killed, or a blob is found damaged later.
// The rule they hold it to: a name in the store either holds exactly the
// bytes of its digest or is reported as damaged; it never hands out other
// bytes, and a failure never costs a blob that was already stored.

// requireSound reopens the store as a restarted daemon does and checks every
// file named by a digest hashes to that digest.
func requireSound(t *testing.T, root string) *Store {
	t.Helper()
	s, err := New(root)
	require.NoError(t, err)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, e := range entries {
		d := Prefix + e.Name()
		if _, ok := ParseDigest(d); !ok {
			continue
		}
		require.NoError(t, s.Verify(d), "the store holds a blob that is not what its name says")
	}
	return s
}

func temps(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if _, ok := ParseDigest(Prefix + e.Name()); !ok {
			out = append(out, e.Name())
		}
	}
	return out
}

// damages are the ways a stored file is found changed after a power cut or
// on a failing disk.
var damages = map[string]func([]byte) []byte{
	"empty":     func([]byte) []byte { return nil },
	"truncated": func(b []byte) []byte { return b[:len(b)/2] },
	"bit flip":  func(b []byte) []byte { c := bytes.Clone(b); c[len(c)/3] ^= 0x01; return c },
	"zeroed":    func(b []byte) []byte { return make([]byte, len(b)) },
	"grown":     func(b []byte) []byte { return append(bytes.Clone(b), "trailing garbage"...) },
}

func damage(t *testing.T, s *Store, digest string, how func([]byte) []byte) {
	t.Helper()
	p, err := s.Path(digest)
	require.NoError(t, err)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(p, 0o644))
	require.NoError(t, os.WriteFile(p, how(data), 0o644))
	require.NoError(t, os.Chmod(p, 0o444))
}

func writeAll(b []byte) func(io.Writer) error {
	return func(w io.Writer) error { _, err := w.Write(b); return err }
}

// A recipe is stored with PutBytes. Whatever step of the write fails, the
// name holds the whole recipe or does not exist, the blobs stored before are
// untouched, and the put can simply be repeated.
func TestFaults_PutBytesFailsAtEveryStep(t *testing.T) {
	recipe := []byte(`{"a recipe":"of some length, so half of it is a recognizable prefix"}`)
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			root := t.TempDir()
			s, err := New(root)
			require.NoError(t, err)
			before, err := s.PutBytes([]byte("stored before the failure"))
			require.NoError(t, err)

			inj := durabletest.Inject(t, f, 0, nil)
			d, err := s.PutBytes(recipe)
			require.True(t, inj.Fired(), "the failure was never reached")
			durable.SetFault(nil)

			s = requireSound(t, root)
			got, rerr := s.ReadBytes(before)
			require.NoError(t, rerr, "a blob stored before the failure was lost")
			assert.Equal(t, "stored before the failure", string(got))
			if err == nil {
				data, rerr := s.ReadBytes(d)
				require.NoError(t, rerr, "PutBytes reported success for a blob that cannot be read")
				assert.Equal(t, recipe, data)
			} else {
				assert.Empty(t, d)
			}
			if !f.Crash() {
				assert.Empty(t, temps(t, root), "a failed write left a temporary file")
			}

			// The same put works once the fault is gone, and a prune clears
			// what a crash left behind without touching a stored blob.
			d, err = s.PutBytes(recipe)
			require.NoError(t, err)
			data, err := s.ReadBytes(d)
			require.NoError(t, err)
			assert.Equal(t, recipe, data)
			_, err = s.Prune(func(string) bool { return true }, 0)
			require.NoError(t, err)
			assert.Empty(t, temps(t, root))
			requireSound(t, root)
		})
	}
}

// A blob whose bytes changed on disk must never be handed out, whatever the
// change, and must not be mistaken for the blob it is named after.
func TestFaults_DamagedBlobIsNeverServed(t *testing.T) {
	content := []byte(strings.Repeat("the bytes of a runtime ", 400))
	for name, how := range damages {
		t.Run(name, func(t *testing.T) {
			s, err := New(t.TempDir())
			require.NoError(t, err)
			d, err := s.PutStream(digestOf(content), writeAll(content))
			require.NoError(t, err)
			intact, err := s.PutBytes([]byte("another blob"))
			require.NoError(t, err)
			damage(t, s, d, how)

			data, err := s.ReadBytes(d)
			require.ErrorContains(t, err, "corrupt")
			assert.Nil(t, data, "the damaged bytes were returned beside the error")
			require.ErrorContains(t, s.Verify(d), "corrupt")
			require.NotErrorIs(t, s.Verify(d), ErrNotFound)

			// The damage does not spread: the other blob still verifies.
			require.NoError(t, s.Verify(intact))
			got, err := s.ReadBytes(intact)
			require.NoError(t, err)
			assert.Equal(t, "another blob", string(got))
		})
	}
}

// Storing a recipe again is how a rebuild or a second pull repairs one a
// power cut left damaged: the put must not trust the name it finds.
func TestFaults_PutBytesReplacesADamagedBlob(t *testing.T) {
	recipe := []byte(strings.Repeat(`{"recipe":true}`, 50))
	for name, how := range damages {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s, err := New(root)
			require.NoError(t, err)
			d, err := s.PutBytes(recipe)
			require.NoError(t, err)
			damage(t, s, d, how)

			again, err := s.PutBytes(recipe)
			require.NoError(t, err)
			require.Equal(t, d, again)
			data, err := s.ReadBytes(d)
			require.NoError(t, err, "the put reported success and left the damaged blob in place")
			assert.Equal(t, recipe, data)
			assert.Empty(t, temps(t, root))
		})
	}
}

// The repair itself can fail. The damaged bytes must not come back as the
// recipe: after a failed repair the name is whole or reported as an error.
func TestFaults_RepairOfADamagedBlobFailsAtEveryStep(t *testing.T) {
	recipe := []byte(strings.Repeat(`{"recipe":true}`, 50))
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			root := t.TempDir()
			s, err := New(root)
			require.NoError(t, err)
			d, err := s.PutBytes(recipe)
			require.NoError(t, err)
			damage(t, s, d, damages["truncated"])

			inj := durabletest.Inject(t, f, 0, nil)
			_, perr := s.PutBytes(recipe)
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			data, rerr := s.ReadBytes(d)
			if perr == nil {
				require.NoError(t, rerr)
			}
			if rerr == nil {
				assert.Equal(t, recipe, data)
			}
			_, err = s.PutBytes(recipe)
			require.NoError(t, err)
			data, err = s.ReadBytes(d)
			require.NoError(t, err)
			assert.Equal(t, recipe, data)
		})
	}
}

// errWriter fails the way a full disk does part of the way through a file.
type failAfter struct {
	w    io.Writer
	left int
	err  error
}

func (f *failAfter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		n, _ := f.w.Write(p[:f.left])
		f.left = 0
		return n, f.err
	}
	f.left -= len(p)
	return f.w.Write(p) //nolint:wrapcheck // a test double passes the error through
}

// A stream that stops half-way (the disk filled up, the device errored, the
// decoder gave up) leaves nothing under the digest and no temporary file.
func TestFaults_PutStreamInterruptedLeavesNothing(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	d := digestOf(content)
	for name, cause := range map[string]error{
		"disk full": syscall.ENOSPC,
		"io error":  syscall.EIO,
		"decoder":   errors.New("zstd: frame is cut short"),
	} {
		for _, at := range []int{0, 1, len(content) / 2, len(content) - 1} {
			t.Run(fmt.Sprintf("%s/after %d bytes", name, at), func(t *testing.T) {
				root := t.TempDir()
				s, err := New(root)
				require.NoError(t, err)
				got, err := s.PutStream(d, func(w io.Writer) error {
					_, err := (&failAfter{w: w, left: at, err: cause}).Write(content)
					return err
				})
				require.ErrorIs(t, err, cause)
				assert.Empty(t, got)
				assert.False(t, s.Has(d), "a partial blob is stored under the digest")
				assert.Empty(t, temps(t, root), "the partial file was left behind")
			})
		}
	}
}

// A writer that reports success after writing something else than the blob:
// too little, too much, or other bytes of the right length.
func TestFaults_PutStreamRefusesWhatDoesNotHashToTheDigest(t *testing.T) {
	content := bytes.Repeat([]byte("blob "), 1000)
	d := digestOf(content)
	for name, how := range damages {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s, err := New(root)
			require.NoError(t, err)
			_, err = s.PutStream(d, writeAll(how(content)))
			require.ErrorContains(t, err, "content changed")
			assert.False(t, s.Has(d))
			assert.Empty(t, temps(t, root))
		})
	}
}

// PutFile hashes the source, copies it, and hashes the copy: a source that
// is missing or unreadable stores nothing.
func TestFaults_PutFileOfAnUnusableSource(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)
	_, err = s.PutFile(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	_, err = s.PutFile(t.TempDir())
	require.Error(t, err, "a directory is not a blob")
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		secret := filepath.Join(t.TempDir(), "unreadable")
		require.NoError(t, os.WriteFile(secret, []byte("x"), 0o000))
		_, err = s.PutFile(secret)
		require.Error(t, err)
	}
	assert.Empty(t, temps(t, root))
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// The store cannot take new blobs when its directory refuses new files, as a
// read-only remount after a disk error does; it still serves what it holds.
func TestFaults_ReadOnlyStoreStillServes(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX directory permissions and a non-root user")
	}
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)
	held, err := s.PutBytes([]byte("held"))
	require.NoError(t, err)
	require.NoError(t, os.Chmod(root, 0o555))
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	_, err = s.PutBytes([]byte("new"))
	require.Error(t, err)
	_, err = s.PutStream(digestOf([]byte("new")), writeAll([]byte("new")))
	require.Error(t, err)
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(src, []byte("new"), 0o644))
	_, err = s.PutFile(src)
	require.Error(t, err)

	data, err := s.ReadBytes(held)
	require.NoError(t, err)
	assert.Equal(t, "held", string(data))
	// Storing what is already held needs no new file.
	_, err = s.PutBytes([]byte("held"))
	require.NoError(t, err)
}

// Blobs are stored read-only whichever way they came in, so nothing that
// opens one for writing by mistake changes a blob other images share.
func TestFaults_BlobsAreReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	s, err := New(t.TempDir())
	require.NoError(t, err)
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(src, []byte("from a file"), 0o755))
	fromFile, err := s.PutFile(src)
	require.NoError(t, err)
	fromBytes, err := s.PutBytes([]byte("from bytes"))
	require.NoError(t, err)
	fromStream, err := s.PutStream(digestOf([]byte("from a stream")), writeAll([]byte("from a stream")))
	require.NoError(t, err)
	for _, d := range []string{fromFile, fromBytes, fromStream} {
		p, _ := s.Path(d)
		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o444), info.Mode().Perm(), d)
	}
}

// A build and a pull, or two pulls, store the same blob at the same time.
// Every one of them must succeed and the blob must be whole.
func TestFaults_ConcurrentPutsOfTheSameDigest(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)
	content := bytes.Repeat([]byte("shared runtime "), 20000)
	d := digestOf(content)
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(src, content, 0o644))

	const each = 8
	errs := make(chan error, 3*each)
	var wg sync.WaitGroup
	start := make(chan struct{})
	run := func(put func() (string, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := put()
			if err == nil && got != d {
				err = fmt.Errorf("stored as %s", got)
			}
			errs <- err
		}()
	}
	for range each {
		run(func() (string, error) { return s.PutBytes(content) })
		run(func() (string, error) { return s.PutStream(d, writeAll(content)) })
		run(func() (string, error) { return s.PutFile(src) })
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	data, err := s.ReadBytes(d)
	require.NoError(t, err)
	assert.Equal(t, content, data)
	assert.Empty(t, temps(t, root))
}

// A prune that runs while a build stores a blob the store already holds, and
// that is old enough to prune, must not delete it after the put has reported
// it stored: the put either refreshes the blob before the prune looks, or
// stores it again after the prune removed it.
func TestFaults_PruneRacingAPutNeverLosesTheBlob(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)
	content := []byte("a blob both an old image and the new build use")
	d := digestOf(content)
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(src, content, 0o644))
	past := time.Now().Add(-2 * time.Hour)

	puts := []func() (string, error){
		func() (string, error) { return s.PutBytes(content) },
		func() (string, error) { return s.PutStream(d, writeAll(content)) },
		func() (string, error) { return s.PutFile(src) },
	}
	for round := range 600 {
		_, err := s.PutBytes(content)
		require.NoError(t, err)
		p, _ := s.Path(d)
		require.NoError(t, os.Chtimes(p, past, past))

		var wg sync.WaitGroup
		var putErr, pruneErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, putErr = puts[round%len(puts)]()
		}()
		go func() {
			defer wg.Done()
			<-start
			_, pruneErr = s.Prune(func(string) bool { return false }, time.Hour)
		}()
		close(start)
		wg.Wait()
		require.NoError(t, putErr)
		require.NoError(t, pruneErr)
		require.True(t, s.Has(d), "round %d: the put reported the blob stored and the prune deleted it", round)
		require.NoError(t, s.Verify(d))
	}
}

// Prune also collects what interrupted writes left, once it is old enough
// not to belong to a write in progress, and nothing else.
func TestFaults_PruneRemovesOldTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)
	kept, err := s.PutBytes([]byte("kept"))
	require.NoError(t, err)
	past := time.Now().Add(-2 * time.Hour)
	write := func(name string, old bool) string {
		p := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(p, []byte("partial"), 0o600))
		if old {
			require.NoError(t, os.Chtimes(p, past, past))
		}
		return p
	}
	oldStream := write(".tmp-123456", true)
	oldDurable := write("."+strings.Repeat("a", 64)+".tmp-99", true)
	inFlight := write(".tmp-654321", false)
	foreign := write("README", true)
	dir := filepath.Join(root, strings.Repeat("b", 64))
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Chtimes(dir, past, past))

	removed, err := s.Prune(func(d string) bool { return d == kept }, time.Hour)
	require.NoError(t, err)
	assert.Empty(t, removed, "only blobs are reported as removed")
	assert.NoFileExists(t, oldStream)
	assert.NoFileExists(t, oldDurable)
	assert.FileExists(t, inFlight, "a write in progress lost its file")
	assert.FileExists(t, foreign)
	assert.DirExists(t, dir)
	require.NoError(t, s.Verify(kept))
}

func TestFaults_PruneOfAMissingStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	s, err := New(root)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(root))
	_, err = s.Prune(func(string) bool { return false }, 0)
	require.Error(t, err)
}

// The daemon is killed while it stores blobs. Whenever that happens, the
// store it leaves holds only whole blobs, and a prune clears the rest.
func TestFaults_StoreSurvivesKill(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)
	base, err := s.PutBytes([]byte("stored before the daemon died"))
	require.NoError(t, err)

	for round := range 12 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperChurnBlobs$") //nolint:gosec // re-executes this test binary
		cmd.Env = append(os.Environ(), "CAS_HELPER_ROOT="+root)
		out, err := cmd.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())
		// The helper reports each blob it stored: kill it after a number of
		// them that changes every round, in the middle of the next writes.
		lines := bufio.NewScanner(out)
		for n := 0; n <= round*3 && lines.Scan(); n++ {
			continue
		}
		require.NoError(t, cmd.Process.Kill())
		_ = cmd.Wait()

		s = requireSound(t, root)
		data, err := s.ReadBytes(base)
		require.NoError(t, err, "round %d", round)
		assert.Equal(t, "stored before the daemon died", string(data))
		_, err = s.Prune(func(d string) bool { return d == base }, 0)
		require.NoError(t, err)
		assert.Empty(t, temps(t, root), "round %d: a prune left files of the killed writes", round)
	}
}

func TestHelperChurnBlobs(t *testing.T) {
	root := os.Getenv("CAS_HELPER_ROOT")
	if root == "" {
		t.Skip("helper process for TestFaults_StoreSurvivesKill")
	}
	s, err := New(root)
	if err != nil {
		os.Exit(3)
	}
	for i := 0; ; i++ {
		content := bytes.Repeat(fmt.Appendf(nil, "churn %d ", i), 30000)
		if i%2 == 0 {
			_, err = s.PutBytes(content)
		} else {
			_, err = s.PutStream(digestOf(content), func(w io.Writer) error {
				for off := 0; off < len(content); off += 4096 {
					if _, err := w.Write(content[off:min(off+4096, len(content))]); err != nil {
						return err
					}
				}
				return nil
			})
		}
		if err != nil {
			os.Exit(3)
		}
		fmt.Println(i)
	}
}

func FuzzParseDigest(f *testing.F) {
	for _, seed := range []string{
		"sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("A", 64),
		"sha256:" + strings.Repeat("a", 63),
		"sha256:" + strings.Repeat("a", 65),
		"sha256:../../../../../../etc/passwd" + strings.Repeat("a", 36),
		"sha512:" + strings.Repeat("a", 64),
		strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("a", 32) + "/" + strings.Repeat("a", 31),
		"sha256:" + strings.Repeat("a", 63) + "\x00",
		"sha256:" + strings.Repeat("é", 32),
		"",
	} {
		f.Add(seed)
	}
	root := f.TempDir()
	s, err := New(root)
	require.NoError(f, err)
	f.Fuzz(func(t *testing.T, digest string) {
		h, ok := ParseDigest(digest)
		p, err := s.Path(digest)
		if !ok {
			require.Error(t, err, "Path accepted a digest ParseDigest refuses")
			require.False(t, s.Has(digest))
			_, err = s.ReadBytes(digest)
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrNotFound, "a malformed digest must not look like a missing blob")
			return
		}
		require.NoError(t, err)
		require.Equal(t, Prefix+h, digest)
		require.Len(t, h, 64)
		require.Equal(t, strings.ToLower(h), h)
		require.Equal(t, root, filepath.Dir(p), "a digest named a path outside the store")
		require.Equal(t, h, filepath.Base(p))
	})
}
