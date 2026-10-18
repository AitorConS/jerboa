package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Store is a content-addressable local image store.
//
// Layout on disk:
//
//	<root>/
//	  <sha256>/
//	    disk.img
//	    manifest.json      (latest manifest stored for this disk)
//	    tags/<ref>.json    (manifest per name:tag)
//	  refs.json            (name:tag → sha256)
//
// Reproducible builds give different tags the same disk, so per-tag metadata
// (name, tag, creation time, default memory, CPUs, ports) lives next to the
// shared disk instead of in a single manifest that the last build overwrote.
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
// The file at diskPath is copied and left untouched.
func (s *Store) Put(name, tag string, m Manifest, diskPath string) error {
	_, err := s.put(name, tag, m, diskPath, false)
	return err
}

// PutMove stores like Put but moves diskPath into the store (rename, falling
// back to copy across filesystems), avoiding a full rewrite of the disk image.
// The file at diskPath is consumed on success. Returns the stored manifest
// with DiskDigest populated.
func (s *Store) PutMove(name, tag string, m Manifest, diskPath string) (Manifest, error) {
	return s.put(name, tag, m, diskPath, true)
}

func (s *Store) put(name, tag string, m Manifest, diskPath string, move bool) (Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	digest, err := fileSHA256(diskPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("image store put: %w", err)
	}
	m.DiskDigest = digest
	m.Name, m.Tag = name, tag

	imgDir := filepath.Join(s.root, stripPrefix(digest))
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return Manifest{}, fmt.Errorf("image store put mkdir: %w", err)
	}
	dst := filepath.Join(imgDir, "disk.img")
	if _, err := os.Stat(dst); err == nil {
		// The same disk is already stored (a reproducible rebuild or another
		// tag): keep the existing file, which VMs may be cloning from.
		if move {
			_ = os.Remove(diskPath)
		}
	} else if !move || os.Rename(diskPath, dst) != nil {
		if err := copyFile(diskPath, dst); err != nil {
			return Manifest{}, fmt.Errorf("image store put disk: %w", err)
		}
	}
	data, err := Marshal(m)
	if err != nil {
		return Manifest{}, fmt.Errorf("image store put manifest: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(imgDir, "manifest.json"), data); err != nil {
		return Manifest{}, fmt.Errorf("image store put write manifest: %w", err)
	}
	ref := name + ":" + tag
	if err := os.MkdirAll(filepath.Join(imgDir, "tags"), 0o755); err != nil {
		return Manifest{}, fmt.Errorf("image store put mkdir: %w", err)
	}
	if err := writeFileAtomic(tagManifestPath(imgDir, ref), data); err != nil {
		return Manifest{}, fmt.Errorf("image store put write tag manifest: %w", err)
	}
	if err := s.addRef(ref, stripPrefix(digest)); err != nil {
		return Manifest{}, fmt.Errorf("image store put ref: %w", err)
	}
	return m, nil
}

// Get returns the manifest and disk path after verifying the actual disk contents.
func (s *Store) Get(ref string) (Manifest, string, error) {
	m, diskPath, err := s.Resolve(ref)
	if err != nil {
		return Manifest{}, "", err
	}
	digest, err := fileSHA256(diskPath)
	if err != nil {
		return Manifest{}, "", err
	}
	if digest != m.DiskDigest {
		return Manifest{}, "", fmt.Errorf("image integrity mismatch: %s", ref)
	}
	return m, diskPath, nil
}

// Resolve pins a reference to its manifest and content-addressed disk path.
// It checks the manifest against the address, but does not read the disk.
// Boot callers must verify the bytes copied to their private boot image before
// launching a VM. Use Get when verified disk contents are needed immediately.
func (s *Store) Resolve(ref string) (Manifest, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sha, name, err := s.resolveRefName(ref)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("image store resolve %s: %w", ref, err)
	}
	m, err := s.readRefManifest(sha, name)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("image store resolve %s: %w", ref, err)
	}
	if m.DiskDigest != "sha256:"+sha {
		return Manifest{}, "", fmt.Errorf("image integrity mismatch: %s", ref)
	}
	return m, filepath.Join(s.root, sha, "disk.img"), nil
}

// List returns the manifest of every name:tag in the store, sorted by ref.
// Tags that share a disk are listed separately, each with its own metadata.
func (s *Store) List() ([]Manifest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	refs, err := s.readRefs()
	if err != nil {
		return nil, fmt.Errorf("image store list: %w", err)
	}
	names := make([]string, 0, len(refs))
	for ref := range refs {
		names = append(names, ref)
	}
	sort.Strings(names)
	out := make([]Manifest, 0, len(names))
	for _, ref := range names {
		m, err := s.readRefManifest(refs[ref], ref)
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
	if _, named := refs[ref]; named {
		delete(refs, ref)
		_ = os.Remove(tagManifestPath(filepath.Join(s.root, sha), ref))
	} else {
		for name, digest := range refs {
			if digest == sha {
				delete(refs, name)
				_ = os.Remove(tagManifestPath(filepath.Join(s.root, sha), name))
			}
		}
	}
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

// resolveRefName resolves ref to a disk digest and, when ref names a tag, the
// full name:tag it matched (empty for a digest reference).
func (s *Store) resolveRefName(ref string) (string, string, error) {
	refs, err := s.readRefs()
	if err != nil {
		return "", "", err
	}
	if sha, ok := refs[ref]; ok {
		return sha, ref, nil
	}
	// Bare name without tag → try :latest.
	if !strings.Contains(ref, ":") {
		if sha, ok := refs[ref+":latest"]; ok {
			return sha, ref + ":latest", nil
		}
	}
	// Try direct sha256 match or prefix.
	if sha, ok := s.findBySHA(refs, ref); ok {
		return sha, "", nil
	}
	return "", "", fmt.Errorf("%s not found", ref)
}

// tagManifestPath is where the manifest of ref is kept inside an image dir.
func tagManifestPath(imgDir, ref string) string {
	return filepath.Join(imgDir, "tags", url.PathEscape(ref)+".json")
}

// readRefManifest reads the manifest stored for ref on disk sha, falling back
// to the disk's manifest.json for images stored before per-tag manifests and
// for digest references (ref == "").
func (s *Store) readRefManifest(sha, ref string) (Manifest, error) {
	if ref != "" {
		data, err := os.ReadFile(tagManifestPath(filepath.Join(s.root, sha), ref))
		if err == nil {
			m, err := Parse(data)
			if err != nil {
				return Manifest{}, fmt.Errorf("parse manifest %s for %s: %w", sha, ref, err)
			}
			return m, nil
		}
		if !os.IsNotExist(err) {
			return Manifest{}, fmt.Errorf("read manifest %s for %s: %w", sha, ref, err)
		}
	}
	m, err := s.readManifest(sha)
	if err != nil || ref == "" {
		return m, err
	}
	// The shared manifest may belong to another tag of the same disk; the ref
	// itself is authoritative for the name and tag.
	if i := strings.LastIndex(ref, ":"); i > 0 {
		m.Name, m.Tag = ref[:i], ref[i+1:]
	}
	return m, nil
}

// writeFileAtomic writes data to path through a temporary file and a rename,
// so readers never see a partial manifest.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err //nolint:wrapcheck // callers add context
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err //nolint:wrapcheck // callers add context
	}
	if err := tmp.Close(); err != nil {
		return err //nolint:wrapcheck // callers add context
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err //nolint:wrapcheck // callers add context
	}
	return os.Rename(tmp.Name(), path) //nolint:wrapcheck // callers add context
}

func (s *Store) findBySHA(refs map[string]string, ref string) (string, bool) {
	needle := strings.TrimPrefix(ref, "sha256:")
	match := ""
	for _, sha := range refs {
		if sha == needle || strings.HasPrefix(sha, needle) {
			if match != "" && match != sha {
				return "", false
			}
			match = sha
		}
	}
	return match, match != ""
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

// PruneUnreferenced deletes stored images that no ref points to and that
// inUse does not claim, and returns the digests it removed ("sha256:<hex>").
//
// Rebuilding a tag moves its ref to the new image but used to leave the old
// image directory behind, unreachable by name, so the store grew with every
// build. inUse receives a digest in "sha256:<hex>" form and must report the
// images a VM still boots from: a stopped VM clones its disk from the store on
// every start.
func (s *Store) PruneUnreferenced(inUse func(digest string) bool) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	refs, err := s.readRefs()
	if err != nil {
		return nil, fmt.Errorf("image store prune: %w", err)
	}
	referenced := make(map[string]bool, len(refs))
	for _, sha := range refs {
		referenced[sha] = true
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("image store prune: %w", err)
	}
	var removed []string
	for _, e := range entries {
		sha := e.Name()
		if !e.IsDir() || !isSHA256Hex(sha) || referenced[sha] {
			continue
		}
		digest := "sha256:" + sha
		if inUse != nil && inUse(digest) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, sha)); err != nil {
			return removed, fmt.Errorf("image store prune %s: %w", digest, err)
		}
		removed = append(removed, digest)
	}
	return removed, nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
