package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store is a content-addressable local image store.
//
// Layout on disk:
//
//	<root>/
//	  <sha256>/
//	    manifest.json
//	    disk.img
//	  refs.json          (name:tag → sha256)
type Store struct {
	root string
	mu   sync.RWMutex
}

// NewStore opens (or creates) a Store rooted at root.
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("image store mkdir %s: %w", root, err)
	}
	return &Store{root: root}, nil
}

// Put stores the disk image at diskPath and its manifest under name:tag.
func (s *Store) Put(name, tag string, m Manifest, diskPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	digest, err := fileSHA256(diskPath)
	if err != nil {
		return fmt.Errorf("image store put: %w", err)
	}
	m.DiskDigest = digest

	imgDir := filepath.Join(s.root, stripPrefix(digest))
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return fmt.Errorf("image store put mkdir: %w", err)
	}
	if err := copyFile(diskPath, filepath.Join(imgDir, "disk.img")); err != nil {
		return fmt.Errorf("image store put disk: %w", err)
	}
	data, err := Marshal(m)
	if err != nil {
		return fmt.Errorf("image store put manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(imgDir, "manifest.json"), data, 0o644); err != nil {
		return fmt.Errorf("image store put write manifest: %w", err)
	}
	if err := s.addRef(name+":"+tag, stripPrefix(digest)); err != nil {
		return fmt.Errorf("image store put ref: %w", err)
	}
	return nil
}

// Get returns the manifest and disk image path for ref (name:tag or sha256:<hex>).
func (s *Store) Get(ref string) (Manifest, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sha, err := s.resolveRef(ref)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("image store get %s: %w", ref, err)
	}
	m, err := s.readManifest(sha)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("image store get %s: %w", ref, err)
	}
	diskPath := filepath.Join(s.root, sha, "disk.img")
	return m, diskPath, nil
}

// List returns all unique manifests in the store.
func (s *Store) List() ([]Manifest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	refs, err := s.readRefs()
	if err != nil {
		return nil, fmt.Errorf("image store list: %w", err)
	}
	seen := make(map[string]bool)
	var out []Manifest
	for _, sha := range refs {
		if seen[sha] {
			continue
		}
		seen[sha] = true
		m, err := s.readManifest(sha)
		if err != nil {
			return nil, fmt.Errorf("image store list: %w", err)
		}
		out = append(out, m)
	}
	return out, nil
}

// Remove removes the ref from the index. Deletes the image dir if no refs remain.
func (s *Store) Remove(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	refs, err := s.readRefs()
	if err != nil {
		return fmt.Errorf("image store remove: %w", err)
	}
	sha, ok := refs[ref]
	if !ok && !strings.Contains(ref, ":") {
		// Bare name without tag → try :latest (mirrors Resolve behavior).
		sha, ok = refs[ref+":latest"]
		if ok {
			ref += ":latest"
		}
	}
	if !ok {
		// Try resolving as sha256 prefix.
		sha, ok = s.findBySHA(refs, ref)
	}
	if !ok {
		return fmt.Errorf("image store remove: %s not found", ref)
	}
	delete(refs, ref)
	if err := s.writeRefs(refs); err != nil {
		return fmt.Errorf("image store remove write refs: %w", err)
	}
	// Remove image dir if no remaining refs point to this sha.
	for _, v := range refs {
		if v == sha {
			return nil
		}
	}
	if err := os.RemoveAll(filepath.Join(s.root, sha)); err != nil {
		return fmt.Errorf("image store remove dir: %w", err)
	}
	return nil
}

// DiskPath returns the absolute path to the disk image for ref.
func (s *Store) DiskPath(ref string) (string, error) {
	_, path, err := s.Get(ref)
	if err != nil {
		return "", fmt.Errorf("image store disk path: %w", err)
	}
	return path, nil
}

func (s *Store) resolveRef(ref string) (string, error) {
	refs, err := s.readRefs()
	if err != nil {
		return "", err
	}
	if sha, ok := refs[ref]; ok {
		return sha, nil
	}
	// Bare name without tag → try :latest.
	if !strings.Contains(ref, ":") {
		if sha, ok := refs[ref+":latest"]; ok {
			return sha, nil
		}
	}
	// Try direct sha256 match or prefix.
	if sha, ok := s.findBySHA(refs, ref); ok {
		return sha, nil
	}
	return "", fmt.Errorf("%s not found", ref)
}

func (s *Store) findBySHA(refs map[string]string, ref string) (string, bool) {
	needle := strings.TrimPrefix(ref, "sha256:")
	for _, sha := range refs {
		if sha == needle || strings.HasPrefix(sha, needle) {
			return sha, true
		}
	}
	return "", false
}

func (s *Store) readManifest(sha string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(s.root, sha, "manifest.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %s: %w", sha, err)
	}
	m, err := Parse(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %s: %w", sha, err)
	}
	return m, nil
}

func (s *Store) addRef(ref, sha string) error {
	refs, err := s.readRefs()
	if err != nil {
		return err
	}
	refs[ref] = sha
	return s.writeRefs(refs)
}

func (s *Store) readRefs() (map[string]string, error) {
	path := filepath.Join(s.root, "refs.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read refs.json: %w", err)
	}
	var refs map[string]string
	if err := json.Unmarshal(data, &refs); err != nil {
		return nil, fmt.Errorf("parse refs.json: %w", err)
	}
	return refs, nil
}

func (s *Store) writeRefs(refs map[string]string) error {
	data, err := json.MarshalIndent(refs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal refs: %w", err)
	}
	path := filepath.Join(s.root, "refs.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write refs.json: %w", err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			_ = err // best effort
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src %s: %w", src, err)
	}
	defer func() {
		if err := in.Close(); err != nil {
			_ = err // best effort
		}
	}()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dst %s: %w", dst, err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			_ = err // best effort
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s→%s: %w", src, dst, err)
	}
	return nil
}

func stripPrefix(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")
}
