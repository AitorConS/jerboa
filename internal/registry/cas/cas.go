// Package cas is the daemon's content-addressed blob store: every file an
// image is built from, and every image recipe, stored once under its SHA-256.
// Builds fill it, pushes upload from it, and pulls fill it before assembling
// an image from its recipe.
package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AitorConS/jerboa/internal/diskclone"
	"github.com/AitorConS/jerboa/internal/durable"
	pkg "github.com/AitorConS/jerboa/internal/package"
)

// Prefix is the digest algorithm prefix used throughout.
const Prefix = "sha256:"

// ErrNotFound reports a digest the store does not hold.
var ErrNotFound = errors.New("cas: blob not found")

// Store keeps blobs as read-only files named by their hex digest.
type Store struct {
	root string
	// mu orders Prune against the puts of this process: a put holds it
	// shared while it decides a blob is already stored or moves a new one
	// into place, and Prune holds it alone while it decides a blob is old
	// and removes it, so a put never reports a blob that Prune is deleting.
	mu sync.RWMutex
}

// New opens (creating if needed) a store rooted at root.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("cas: create %s: %w", root, err)
	}
	return &Store{root: root}, nil
}

// Root returns the store directory.
func (s *Store) Root() string { return s.root }

// ParseDigest returns the 64-character hex part of "sha256:<hex>" and whether
// the digest is well formed.
func ParseDigest(digest string) (string, bool) {
	h, ok := strings.CutPrefix(digest, Prefix)
	if !ok || len(h) != 64 {
		return "", false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return h, true
}

// Path returns where the blob for digest lives, whether or not it exists.
func (s *Store) Path(digest string) (string, error) {
	h, ok := ParseDigest(digest)
	if !ok {
		return "", fmt.Errorf("cas: invalid digest %q", digest)
	}
	return filepath.Join(s.root, h), nil
}

// Has reports whether the store holds digest.
func (s *Store) Has(digest string) bool {
	p, err := s.Path(digest)
	if err != nil {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// PutFile adds the file at src and returns its digest. The copy is a clone
// where the file system supports it, so adding a large runtime costs no extra
// space there. The stored bytes are hashed after copying, so a source that
// changes during the copy is never stored under a stale digest.
func (s *Store) PutFile(src string) (string, error) {
	digest, err := pkg.FileDigest(src)
	if err != nil {
		return "", fmt.Errorf("cas: digest %s: %w", src, err)
	}
	if s.reuse(digest) {
		return digest, nil
	}
	tmp, err := s.tempPath()
	if err != nil {
		return "", err
	}
	if _, err := diskclone.Clone(tmp, src); err != nil {
		return "", fmt.Errorf("cas: copy %s: %w", src, err)
	}
	return s.commit(tmp, digest)
}

// PutBytes adds data and returns its digest. The blob is flushed to stable
// storage before it takes its name, so a recipe survives a power cut whole or
// not at all; a blob found under the digest with other bytes, as a power cut
// or a failing disk leaves, is replaced.
func (s *Store) PutBytes(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := Prefix + hex.EncodeToString(sum[:])
	dst := filepath.Join(s.root, hex.EncodeToString(sum[:]))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Has(digest) {
		if stored, err := os.ReadFile(dst); err == nil && bytes.Equal(stored, data) { //nolint:gosec // store-owned path
			s.touch(digest)
			return digest, nil
		}
		if err := removeBlob(dst); err != nil {
			return "", fmt.Errorf("cas: replace damaged %s: %w", digest, err)
		}
	}
	if err := durable.WriteFile(dst, data, 0o444); err != nil {
		// Another writer storing the same bytes may have won the name.
		if stored, rerr := os.ReadFile(dst); rerr == nil && bytes.Equal(stored, data) { //nolint:gosec // store-owned path
			return digest, nil
		}
		return "", fmt.Errorf("cas: write: %w", err)
	}
	return digest, nil
}

// PutStream adds the blob write produces, which the caller expects to be
// digest. The bytes go straight to a file, so a large blob never sits in
// memory; they are hashed again before the blob is stored, so a wrong or
// partial write is never kept.
func (s *Store) PutStream(digest string, write func(io.Writer) error) (string, error) {
	if _, err := s.Path(digest); err != nil {
		return "", err
	}
	if s.reuse(digest) {
		return digest, nil
	}
	tmp, err := s.tempPath()
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // a temp file this store created
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("cas: open: %w", err)
	}
	err = write(f)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("cas: write: %w", cerr)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return s.commit(tmp, digest)
}

// ReadBytes returns the contents of a blob, verified against its digest.
func (s *Store) ReadBytes(digest string) ([]byte, error) {
	p, err := s.Path(digest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p) //nolint:gosec // store-owned path derived from a validated digest
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, digest)
	}
	if err != nil {
		return nil, fmt.Errorf("cas: read %s: %w", digest, err)
	}
	sum := sha256.Sum256(data)
	if Prefix+hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("cas: blob %s is corrupt", digest)
	}
	return data, nil
}

// Verify rehashes a stored blob and reports whether it still matches.
func (s *Store) Verify(digest string) error {
	p, err := s.Path(digest)
	if err != nil {
		return err
	}
	if !s.Has(digest) {
		return fmt.Errorf("%w: %s", ErrNotFound, digest)
	}
	got, err := diskclone.HashFile(p)
	if err != nil {
		return fmt.Errorf("cas: hash %s: %w", digest, err)
	}
	if got != digest {
		return fmt.Errorf("cas: blob %s is corrupt", digest)
	}
	return nil
}

// DiscardDamaged removes a stored blob whose bytes no longer match its
// digest, so the next put stores it again instead of reusing the damaged
// file. It reports whether it removed one; a blob that is intact or absent
// is left alone. No put of the same blob runs between the check and the
// removal.
func (s *Store) DiscardDamaged(digest string) (bool, error) {
	p, err := s.Path(digest)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Has(digest) {
		return false, nil
	}
	got, err := diskclone.HashFile(p)
	if err != nil {
		return false, fmt.Errorf("cas: hash %s: %w", digest, err)
	}
	if got == digest {
		return false, nil
	}
	if err := removeBlob(p); err != nil {
		return false, fmt.Errorf("cas: remove damaged %s: %w", digest, err)
	}
	return true, nil
}

// Prune removes blobs that keep rejects and that were added more than grace
// ago. The grace period protects blobs a concurrent build or pull has stored
// but not yet referenced from a recipe. It also removes the temporary files
// of writes that a crash interrupted more than grace ago; those are not in
// the list it returns.
func (s *Store) Prune(keep func(digest string) bool, grace time.Duration) ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("cas: list: %w", err)
	}
	var removed []string
	cutoff := time.Now().Add(-grace)
	for _, e := range entries {
		digest := Prefix + e.Name()
		_, isBlob := ParseDigest(digest)
		if !isBlob && !durable.IsTemp(e.Name()) {
			continue
		}
		if isBlob && keep(digest) {
			continue
		}
		gone, err := s.removeIfOld(filepath.Join(s.root, e.Name()), cutoff)
		if err != nil {
			return removed, fmt.Errorf("cas: remove %s: %w", e.Name(), err)
		}
		if gone && isBlob {
			removed = append(removed, digest)
		}
	}
	return removed, nil
}

// removeIfOld removes path unless it was written or reused after cutoff. It
// looks at the file and removes it with no put in between.
func (s *Store) removeIfOld(path string, cutoff time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() || info.ModTime().After(cutoff) {
		return false, nil //nolint:nilerr // a file that vanished or cannot be read is left for the next prune
	}
	if err := removeBlob(path); err != nil {
		return false, err
	}
	return true, nil
}

// removeBlob deletes a file of the store; one already gone is not an error.
func removeBlob(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Windows refuses to delete a read-only file.
		_ = os.Chmod(path, 0o600) //nolint:gosec // store-owned path, about to be removed
		err = os.Remove(path)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err //nolint:wrapcheck // callers name the blob
	}
	return nil
}

// reuse reports whether the store already holds digest, marking it as
// recently used when it does.
func (s *Store) reuse(digest string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.Has(digest) {
		return false
	}
	s.touch(digest)
	return true
}

// touch marks a blob as recently used, restarting its prune grace period.
func (s *Store) touch(digest string) {
	if p, err := s.Path(digest); err == nil {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
	}
}

func (s *Store) tempPath() (string, error) {
	f, err := os.CreateTemp(s.root, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("cas: temp: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	return name, nil
}

// commit verifies tmp holds digest and moves it into place, read-only.
func (s *Store) commit(tmp, digest string) (string, error) {
	defer func() { _ = os.Remove(tmp) }()
	got, err := diskclone.HashFile(tmp)
	if err != nil {
		return "", fmt.Errorf("cas: hash: %w", err)
	}
	if got != digest {
		return "", fmt.Errorf("cas: content changed while it was stored (got %s, want %s)", got, digest)
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		return "", fmt.Errorf("cas: chmod: %w", err)
	}
	dst, _ := s.Path(digest)
	// Touch so a blob reused by a new build is not pruned during its grace.
	now := time.Now()
	_ = os.Chtimes(tmp, now, now)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := os.Rename(tmp, dst); err != nil {
		if s.Has(digest) {
			return digest, nil
		}
		return "", fmt.Errorf("cas: store %s: %w", digest, err)
	}
	return digest, nil
}
