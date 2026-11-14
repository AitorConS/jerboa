//go:build linux || darwin

package diskclone

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// limitFileSize makes every write past limit bytes of a file fail with EFBIG
// until the test ends: the closest a test gets to a disk that fills up in the
// middle of a copy. The limit is process-wide, so tests using it must not run
// in parallel.
func limitFileSize(t *testing.T, limit uint64) {
	t.Helper()
	var prev unix.Rlimit
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_FSIZE, &prev))
	// Past the limit the kernel also sends SIGXFSZ, which ends the process
	// unless it is ignored.
	signal.Ignore(syscall.SIGXFSZ)
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: limit, Max: prev.Max}))
	t.Cleanup(func() {
		_ = unix.Setrlimit(unix.RLIMIT_FSIZE, &prev)
		signal.Reset(syscall.SIGXFSZ)
	})
}

func TestFaults_CopyCutShortLeavesNoDestination(t *testing.T) {
	for name, chunks := range map[string]map[int64][]byte{
		// The write of a data block fails.
		"data past the limit": {0: []byte("start"), 3 << 20: []byte("data the disk has no room for")},
		// Every write fits, but the file cannot be extended to its size.
		"hole past the limit": {0: []byte("start")},
	} {
		t.Run(name, func(t *testing.T) {
			withoutClone(t)
			dir := t.TempDir()
			src := filepath.Join(dir, "disk.img")
			want := writeSparseImage(t, src, 4<<20, chunks)
			dst := filepath.Join(dir, "rootfs.img")
			direct := filepath.Join(dir, "direct.img")

			limitFileSize(t, 1<<20)
			_, err := Clone(dst, src)
			require.ErrorIs(t, err, syscall.EFBIG)
			// A truncated disk that looks like a finished one is the worst
			// outcome: nothing may be left at the destination.
			requireGone(t, dst)
			require.ErrorIs(t, SparseCopy(direct, src), syscall.EFBIG)
			requireGone(t, direct)

			// Room comes back and the same call succeeds, whole.
			require.NoError(t, unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: unix.RLIM_INFINITY, Max: unix.RLIM_INFINITY}))
			_, err = Clone(dst, src)
			require.NoError(t, err)
			got, err := os.ReadFile(dst)
			require.NoError(t, err)
			require.Len(t, got, len(want))
			assert.Equal(t, want[3<<20:3<<20+32], got[3<<20:3<<20+32])
		})
	}
}

func TestFaults_SourceIsAFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "disk.img")
	require.NoError(t, unix.Mkfifo(fifo, 0o600))
	dst := filepath.Join(dir, "rootfs.img")

	// Opening a FIFO waits for a writer that never comes; the call must
	// refuse it instead of hanging a VM start forever.
	for name, call := range map[string]func() error{
		"clone": func() error { _, err := Clone(dst, fifo); return err },
		"copy":  func() error { return SparseCopy(dst, fifo) },
		"hash":  func() error { _, err := HashFile(fifo); return err },
	} {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			require.ErrorContains(t, err, "not a regular file", name)
		case <-time.After(10 * time.Second):
			// Unblock the goroutine before failing.
			if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
			t.Fatalf("%s hung on a FIFO", name)
		}
		requireGone(t, dst)
	}
}

func TestFaults_HardLinkedDestinationIsReplacedNotWrittenThrough(t *testing.T) {
	for _, clone := range []bool{true, false} {
		if !clone {
			withoutClone(t)
		}
		dir := t.TempDir()
		src := filepath.Join(dir, "disk.img")
		require.NoError(t, os.WriteFile(src, []byte("image content"), 0o600))
		dst := filepath.Join(dir, "rootfs.img")
		// An earlier version, or an operator, linked the VM's disk to the
		// image instead of copying it.
		require.NoError(t, os.Link(src, dst))

		_, err := Clone(dst, src)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dst, []byte("guest wrote here"), 0o600))
		data, err := os.ReadFile(src)
		require.NoError(t, err)
		assert.Equal(t, "image content", string(data), "the copy still shares its bytes with the image")
	}
}
