// Package cas is the daemon's content-addressed blob store: every file an
// image is built from, and every image recipe, stored once under its SHA-256.
// Builds fill it, pushes upload from it, and pulls fill it before assembling
// an image from its recipe.
package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/diskclone"
	pkg "github.com/AitorConS/jerboa/internal/package"
)

// Prefix is the digest algorithm prefix used throughout.
const Prefix = "sha256:"

// ErrNotFound reports a digest the store does not hold.
var ErrNotFound = errors.New("cas: blob not found")

// Store keeps blobs as read-only files named by their hex digest.
type Store struct {
	root string
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
	if s.Has(digest) {
		s.touch(digest)
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

// PutBytes adds data and returns its digest.
func (s *Store) PutBytes(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := Prefix + hex.EncodeToString(sum[:])
	if s.Has(digest) {
		s.touch(digest)
		return digest, nil
	}
	tmp, err := s.tempPath()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("cas: write: %w", err)
	}
	return s.commit(tmp, digest)
}

// PutStream adds the blob write produces, which the caller expects to be
// digest. The bytes go straight to a file, so a large blob never sits in
// memory; they are hashed again before the blob is stored, so a wrong or
// partial write is never kept.
func (s *Store) PutStream(digest string, write func(io.Writer) error) (string, error) {
	if _, err := s.Path(digest); err != nil {
		return "", err
	}
	if s.Has(digest) {
		s.touch(digest)
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

// Prune removes blobs that keep rejects and that were added more than grace
// ago. The grace period protects blobs a concurrent build or pull has stored
// but not yet referenced from a recipe.
func (s *Store) Prune(keep func(digest string) bool, grace time.Duration) ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("cas: list: %w", err)
	}
	var removed []string
	cutoff := time.Now().Add(-grace)
	for _, e := range entries {
		digest := Prefix + e.Name()
		if _, ok := ParseDigest(digest); !ok || keep(digest) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.root, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("cas: remove %s: %w", digest, err)
		}
		removed = append(removed, digest)
	}
	return removed, nil
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
	if err := os.Rename(tmp, dst); err != nil {
		if s.Has(digest) {
			return digest, nil
		}
		return "", fmt.Errorf("cas: store %s: %w", digest, err)
	}
	return digest, nil
}
