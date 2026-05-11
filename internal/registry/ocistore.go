package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/AitorConS/unikernel-engine/internal/ociregistry"
)

// OCIStore persists OCI manifest refs and bodies on disk.
type OCIStore struct {
	root string
	mu   sync.Mutex
}

// NewOCIStore creates an OCIStore rooted at root.
func NewOCIStore(root string) (*OCIStore, error) {
	if err := os.MkdirAll(filepath.Join(root, "manifests"), 0o755); err != nil {
		return nil, fmt.Errorf("OCI store mkdir: %w", err)
	}
	return &OCIStore{root: root}, nil
}

// Save stores a manifest body and updates refs for name/tag and name/digest.
func (s *OCIStore) Save(name, ref, digest string, m ociregistry.Manifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeManifest(digest, m); err != nil {
		return err
	}
	refs, err := s.readRefs()
	if err != nil {
		return err
	}
	if _, ok := refs[name]; !ok {
		refs[name] = make(map[string]string)
	}
	refs[name][ref] = digest
	refs[name][digest] = digest
	return s.writeRefs(refs)
}

// Get returns a manifest for name/ref.
func (s *OCIStore) Get(name, ref string) (ociregistry.Manifest, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	refs, err := s.readRefs()
	if err != nil {
		return ociregistry.Manifest{}, "", err
	}
	repo, ok := refs[name]
	if !ok {
		return ociregistry.Manifest{}, "", fmt.Errorf("repository not found")
	}
	digest, ok := repo[ref]
	if !ok {
		return ociregistry.Manifest{}, "", fmt.Errorf("manifest not found")
	}
	m, err := s.readManifest(digest)
	if err != nil {
		return ociregistry.Manifest{}, "", err
	}
	return m, digest, nil
}

// Delete removes a ref for a repository.
func (s *OCIStore) Delete(name, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	refs, err := s.readRefs()
	if err != nil {
		return err
	}
	repo, ok := refs[name]
	if !ok {
		return fmt.Errorf("repository not found")
	}
	if _, ok := repo[ref]; !ok {
		return fmt.Errorf("manifest not found")
	}
	delete(repo, ref)
	if len(repo) == 0 {
		delete(refs, name)
	} else {
		refs[name] = repo
	}
	return s.writeRefs(refs)
}

// Repositories returns known repository names.
func (s *OCIStore) Repositories() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	refs, err := s.readRefs()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for name := range refs {
		out = append(out, name)
	}
	return out, nil
}

// ReferencedDigests returns blob digests referenced by stored manifests.
func (s *OCIStore) ReferencedDigests() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	refs, err := s.readRefs()
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{})
	for _, repo := range refs {
		for _, manifestDigest := range repo {
			m, err := s.readManifest(manifestDigest)
			if err != nil {
				continue
			}
			set[m.Config.Digest] = struct{}{}
			for _, layer := range m.Layers {
				set[layer.Digest] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for digest := range set {
		out = append(out, digest)
	}
	return out, nil
}

func (s *OCIStore) refsPath() string {
	return filepath.Join(s.root, "refs.json")
}

func (s *OCIStore) readRefs() (map[string]map[string]string, error) {
	data, err := os.ReadFile(s.refsPath())
	if os.IsNotExist(err) {
		return map[string]map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read OCI refs: %w", err)
	}
	var refs map[string]map[string]string
	if err := json.Unmarshal(data, &refs); err != nil {
		return nil, fmt.Errorf("parse OCI refs: %w", err)
	}
	return refs, nil
}

func (s *OCIStore) writeRefs(refs map[string]map[string]string) error {
	data, err := json.MarshalIndent(refs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal OCI refs: %w", err)
	}
	if err := os.WriteFile(s.refsPath(), data, 0o644); err != nil {
		return fmt.Errorf("write OCI refs: %w", err)
	}
	return nil
}

func (s *OCIStore) writeManifest(digest string, m ociregistry.Manifest) error {
	data, err := ociregistry.MarshalManifest(m)
	if err != nil {
		return fmt.Errorf("marshal OCI manifest: %w", err)
	}
	path := filepath.Join(s.root, "manifests", digestToFilename(digest))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write OCI manifest: %w", err)
	}
	return nil
}

func (s *OCIStore) readManifest(digest string) (ociregistry.Manifest, error) {
	path := filepath.Join(s.root, "manifests", digestToFilename(digest))
	data, err := os.ReadFile(path)
	if err != nil {
		return ociregistry.Manifest{}, fmt.Errorf("read OCI manifest: %w", err)
	}
	m, err := ociregistry.ParseManifest(data)
	if err != nil {
		return ociregistry.Manifest{}, fmt.Errorf("parse OCI manifest: %w", err)
	}
	return m, nil
}

func digestToFilename(digest string) string {
	return strings.ReplaceAll(digest, ":", "_") + ".json"
}
