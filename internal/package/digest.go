package pkg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Package files are hashed by several checks in one build (package
// verification, collision checks, the image's content UUID) and again by every
// later build. fileDigest remembers each file's SHA-256 against the file's
// identity, and SeedDigests/SaveDigests keep those digests next to a package
// across processes, so an unchanged file is read once.

// fileID identifies a file's contents without reading it: any write, truncate
// or replacement changes it, and the status-change time cannot be set back.
type fileID struct {
	Size     int64  `json:"size"`
	ModNs    int64  `json:"mod_ns"`
	ChangeNs int64  `json:"change_ns"`
	Ino      uint64 `json:"ino"`
}

type digestKey struct {
	path string
	id   fileID
}

var digestCache sync.Map // digestKey -> [32]byte

// fileDigest returns the SHA-256 of the file at path, reading it only when no
// digest is known for the file's current identity.
func fileDigest(path string) ([32]byte, error) {
	id, err := statID(path)
	if err != nil {
		return [32]byte{}, err
	}
	key := digestKey{path, id}
	if v, ok := digestCache.Load(key); ok {
		return v.([32]byte), nil //nolint:forcetypeassert // only [32]byte is stored
	}
	f, err := os.Open(path) //nolint:gosec // package store and build context paths
	if err != nil {
		return [32]byte{}, fmt.Errorf("hash %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, fmt.Errorf("hash %s: %w", path, err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	// Store only if the file did not change while it was read.
	if after, err := statID(path); err == nil && after == id {
		digestCache.Store(key, sum)
	}
	return sum, nil
}

// FileDigest is fileDigest as a "sha256:<hex>" string, for callers outside
// this package that hash the same files (the image content UUID).
func FileDigest(path string) (string, error) {
	sum, err := fileDigest(path)
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

const digestsFile = ".digests.json"

type digestRecord struct {
	ID     fileID `json:"id"`
	SHA256 string `json:"sha256"`
}

type digestIndex struct {
	// Files maps a path relative to the package directory to its digest.
	Files map[string]digestRecord `json:"files"`
	// VerifiedArchive is the archive SHA-256 the extracted files were last
	// checked against, valid while every recorded file is unchanged.
	VerifiedArchive string `json:"verified_archive,omitempty"`
}

func readDigestIndex(dir string) digestIndex {
	var idx digestIndex
	data, err := os.ReadFile(filepath.Join(dir, digestsFile))
	if err != nil || json.Unmarshal(data, &idx) != nil || idx.Files == nil {
		return digestIndex{Files: map[string]digestRecord{}}
	}
	return idx
}

// SeedDigests loads the digests saved in dir for files that are still the
// same file, so this process does not read them again.
func SeedDigests(dir string) {
	idx := readDigestIndex(dir)
	for rel, rec := range idx.Files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		id, err := statID(path)
		if err != nil || id != rec.ID || (id.Ino == 0 && id.ChangeNs == 0) {
			continue // changed, or no identity strong enough to trust across processes
		}
		raw, err := hex.DecodeString(rec.SHA256)
		if err != nil || len(raw) != 32 {
			continue
		}
		var sum [32]byte
		copy(sum[:], raw)
		digestCache.Store(digestKey{path, id}, sum)
	}
}

// SaveDigests records in dir the known digests of the given files that live
// under dir. verifiedArchive, when set, notes that these files were checked
// against that archive digest.
func SaveDigests(dir string, files []File, verifiedArchive string) error {
	idx := digestIndex{Files: map[string]digestRecord{}, VerifiedArchive: verifiedArchive}
	for _, f := range files {
		if f.IsDir || f.HostPath == "" {
			continue
		}
		rel, err := filepath.Rel(dir, f.HostPath)
		if err != nil || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
			continue
		}
		id, err := statID(f.HostPath)
		if err != nil {
			continue
		}
		v, ok := digestCache.Load(digestKey{f.HostPath, id})
		if !ok {
			continue
		}
		sum := v.([32]byte) //nolint:forcetypeassert // only [32]byte is stored
		idx.Files[filepath.ToSlash(rel)] = digestRecord{ID: id, SHA256: hex.EncodeToString(sum[:])}
	}
	data, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("save digests: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".digests-*")
	if err != nil {
		return fmt.Errorf("save digests: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("save digests: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save digests: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, digestsFile)); err != nil {
		return fmt.Errorf("save digests: %w", err)
	}
	return nil
}

// archiveVerified reports whether dir's files were checked against the
// archive with digest sha and none of them changed since.
func archiveVerified(dir, sha string, files []File) bool {
	idx := readDigestIndex(dir)
	if sha == "" || idx.VerifiedArchive != sha {
		return false
	}
	n := 0
	for _, f := range files {
		if f.IsDir {
			continue
		}
		rel, err := filepath.Rel(dir, f.HostPath)
		if err != nil {
			return false
		}
		rec, ok := idx.Files[filepath.ToSlash(rel)]
		if !ok {
			return false
		}
		if id, err := statID(f.HostPath); err != nil || id != rec.ID {
			return false
		}
		n++
	}
	return n > 0
}
