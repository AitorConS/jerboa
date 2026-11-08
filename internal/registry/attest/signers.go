package attest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/AitorConS/jerboa/internal/naming"
)

// Signers are the workflows a client remembers as the signers of
// repositories: after the first pull of a repository that verifies, later
// pulls require the same workflow, so neither a change of signer nor
// removing the signatures goes unnoticed (trust on first use, as SSH does
// with host keys). The file is ~/.jerboa/signers.json.
type Signers struct {
	path string
	m    map[string]string
}

type signersFile struct {
	Signers map[string]string `json:"signers"`
}

// SignerKey names a repository in the file: registry/namespace/repository.
func SignerKey(ref naming.ImageReference) string {
	return ref.Registry + "/" + ref.Namespace + "/" + ref.Repository
}

// LoadSigners reads the file at path; a missing file remembers nobody.
func LoadSigners(path string) (*Signers, error) {
	s := &Signers{path: path, m: map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("signers: %w", err)
	}
	var f signersFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("signers: %s: %w", path, err)
	}
	for k, v := range f.Signers {
		if _, err := ParseIdentity(v); err != nil {
			return nil, fmt.Errorf("signers: %s: %s: %w", path, k, err)
		}
		s.m[k] = v
	}
	return s, nil
}

// Get returns the signer remembered for the repository ref names.
func (s *Signers) Get(ref naming.ImageReference) (Identity, bool) {
	v, ok := s.m[SignerKey(ref)]
	if !ok {
		return Identity{}, false
	}
	id, err := ParseIdentity(v)
	return id, err == nil
}

// Remember records id, without its ref, as the signer of the repository ref
// names, and saves the file.
func (s *Signers) Remember(ref naming.ImageReference, id Identity) error {
	id.Ref = ""
	s.m[SignerKey(ref)] = id.String()
	return s.save()
}

// Forget drops what is remembered for key (registry/namespace/repository),
// and reports whether there was anything.
func (s *Signers) Forget(key string) (bool, error) {
	if _, ok := s.m[key]; !ok {
		return false, nil
	}
	delete(s.m, key)
	return true, s.save()
}

// List returns the remembered repositories and their signers, sorted.
func (s *Signers) List() [][2]string {
	out := make([][2]string, 0, len(s.m))
	for k, v := range s.m {
		out = append(out, [2]string{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func (s *Signers) save() error {
	data, err := json.MarshalIndent(signersFile{Signers: s.m}, "", "  ")
	if err != nil {
		return fmt.Errorf("signers: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("signers: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".signers-*.json")
	if err != nil {
		return fmt.Errorf("signers: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("signers: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("signers: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("signers: %w", err)
	}
	return nil
}
