// Package diskclone creates private, writable copies of VM disk images without
// writing the whole image on every VM start.
//
// Clone first asks the filesystem for a copy-on-write clone (FICLONE on Linux
// btrfs/XFS/bcachefs, clonefile on APFS), which is O(metadata) regardless of
// image size. When the filesystem cannot clone (ext4, tmpfs, cross-device
// paths) it falls back to a sparse copy that skips holes and all-zero blocks,
// so a 1 GiB image with a few MiB of data costs a few MiB of writes instead of
// a full gigabyte.
//
// HashFile computes the SHA-256 of a file while treating holes as zeros without
// reading them, so verifying a sparse private copy stays cheap.
package diskclone

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Method reports how a private copy was produced.
type Method string

const (
	// MethodClone is a filesystem copy-on-write clone (reflink/clonefile).
	MethodClone Method = "clone"
	// MethodSparseCopy is a hole-preserving byte copy.
	MethodSparseCopy Method = "sparse-copy"
)

// blockSize is the granularity at which the sparse copy detects zero blocks.
// It is a multiple of common filesystem block sizes, so every skipped block is
// one the filesystem can leave unallocated.
const blockSize = 1 << 16

// zeroBlock is a shared all-zero buffer used for zero detection and for
// hashing holes without reading them.
var zeroBlock = make([]byte, blockSize)

// tryClone is the platform's copy-on-write clone. It is a variable so tests
// can take it away, as a filesystem without reflinks does.
var tryClone = cloneFile

// Clone creates dst as an independent copy of src with mode 0600, replacing
// any existing dst. Writes to dst never affect src. On error dst is removed.
func Clone(dst, src string) (method Method, err error) {
	// Before anything is removed: replacing dst must not delete the image
	// when both paths name the same file.
	if sameEntry(dst, src) {
		return "", fmt.Errorf("diskclone: %s is the source itself", dst)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("diskclone: remove stale %s: %w", dst, err)
	}
	// Clone the image a symlink points to, never the link: a cloned link is
	// a second name for the image, and the VM would write to the image.
	if err := regularFile(src); err != nil {
		return "", err
	}
	if st, lerr := os.Lstat(src); lerr == nil && st.Mode()&os.ModeSymlink != 0 {
		if src, err = filepath.EvalSymlinks(src); err != nil {
			return "", fmt.Errorf("diskclone: resolve %s: %w", src, err)
		}
	}
	if tryClone(dst, src) {
		if err := os.Chmod(dst, 0o600); err != nil {
			return "", fmt.Errorf("diskclone: chmod %s: %w", dst, err)
		}
		return MethodClone, nil
	}
	if err := SparseCopy(dst, src); err != nil {
		return "", err
	}
	return MethodSparseCopy, nil
}

// SparseCopy copies src to dst (mode 0600, replacing any existing dst), leaving holes for source
// holes and for all-zero blocks. The result reads back byte-identical to src.
func SparseCopy(dst, src string) (err error) {
	if sameEntry(dst, src) {
		return fmt.Errorf("diskclone: %s is the source itself", dst)
	}
	if err := regularFile(src); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // daemon-owned image store path
	if err != nil {
		return fmt.Errorf("diskclone: open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return fmt.Errorf("diskclone: stat %s: %w", src, err)
	}
	// Create dst afresh: O_EXCL refuses a file or symlink planted at the path
	// between the remove and the open, so the copy never writes through it.
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("diskclone: remove stale %s: %w", dst, err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // caller-owned path
	if err != nil {
		return fmt.Errorf("diskclone: create %s: %w", dst, err)
	}
	defer func() {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("diskclone: close %s: %w", dst, cerr)
		}
		// A copy cut short (full disk, I/O error) is not left behind for a
		// later run to take for a disk.
		if err != nil {
			_ = os.Remove(dst)
		}
	}()

	size := st.Size()
	buf := make([]byte, blockSize)
	err = forEachDataRange(in, size, func(start, end int64) error {
		for off := start; off < end; {
			n := int64(len(buf))
			if end-off < n {
				n = end - off
			}
			if _, err := io.ReadFull(io.NewSectionReader(in, off, n), buf[:n]); err != nil {
				return fmt.Errorf("diskclone: read %s: %w", src, err)
			}
			if !bytes.Equal(buf[:n], zeroBlock[:n]) {
				if _, err := out.WriteAt(buf[:n], off); err != nil {
					return fmt.Errorf("diskclone: write %s: %w", dst, err)
				}
			}
			off += n
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Extends the file over trailing holes/zero blocks without allocating them.
	if err := out.Truncate(size); err != nil {
		return fmt.Errorf("diskclone: truncate %s: %w", dst, err)
	}
	return nil
}

// HashFile returns the "sha256:<hex>" digest of path. Holes are hashed as the
// zeros they read back as, without reading them from disk.
func HashFile(path string) (string, error) {
	if err := regularFile(path); err != nil {
		return "", err
	}
	f, err := os.Open(path) //nolint:gosec // caller-owned path
	if err != nil {
		return "", fmt.Errorf("diskclone: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("diskclone: stat %s: %w", path, err)
	}
	h := sha256.New()
	var hashed int64
	err = forEachDataRange(f, st.Size(), func(start, end int64) error {
		hashZeros(h, start-hashed)
		if _, err := io.Copy(h, io.NewSectionReader(f, start, end-start)); err != nil {
			return fmt.Errorf("diskclone: read %s: %w", path, err)
		}
		hashed = end
		return nil
	})
	if err != nil {
		return "", err
	}
	hashZeros(h, st.Size()-hashed)
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

func hashZeros(h hash.Hash, n int64) {
	for n > 0 {
		chunk := int64(len(zeroBlock))
		if n < chunk {
			chunk = n
		}
		_, _ = h.Write(zeroBlock[:chunk])
		n -= chunk
	}
}

// regularFile reports why path cannot be copied or hashed: it must be a
// regular file, directly or through symlinks. A directory has no bytes to
// copy (some filesystems clone one, or report it as all holes), and opening a
// FIFO or a device would block or read without end.
func regularFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("diskclone: open %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("diskclone: %s is not a regular file", path)
	}
	return nil
}

// sameEntry reports whether dst and src are the same name in the same
// directory, however the two paths are spelled. A hard link to the source or
// a symlink to it is another entry: removing it leaves the source in place.
func sameEntry(dst, src string) bool {
	if !strings.EqualFold(filepath.Base(dst), filepath.Base(src)) {
		return false
	}
	d, err := os.Lstat(dst)
	if err != nil {
		return false
	}
	s, err := os.Lstat(src)
	if err != nil || !os.SameFile(d, s) {
		return false
	}
	dd, err := os.Stat(filepath.Dir(dst))
	if err != nil {
		return false
	}
	sd, err := os.Stat(filepath.Dir(src))
	return err == nil && os.SameFile(dd, sd)
}
