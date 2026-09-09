package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Spec is the input to manifest generation: it names each component's version
// and the local files that make it up. The generator hashes those files and
// derives their published URLs from the bucket convention
// <base>/<component>/<version>/<basename>. Desktop uses
// <base>/desktop/<basename>: electron-updater already uses versioned filenames
// in that directory, owned exclusively by the Desktop release pipeline.
type Spec struct {
	Channel    string                   `json:"channel"`
	Base       string                   `json:"base"`
	Components map[string]SpecComponent `json:"components"`
}

// SpecComponent is one component in a Spec. Exactly one of File, Platforms, or
// Files describes its artifacts.
type SpecComponent struct {
	Version string `json:"version"`

	File      string            `json:"file,omitempty"`      // single-file component
	Platforms map[string]string `json:"platforms,omitempty"` // platform -> local path
	Files     map[string]string `json:"files,omitempty"`     // logical name -> local path

	// Compatibility passthrough.
	MinCLI    string `json:"min_cli,omitempty"`
	Proto     int    `json:"proto,omitempty"`
	KernelVer string `json:"kernel,omitempty"`
}

// BuildManifest turns a Spec into a signed-ready Manifest by hashing each local
// file and deriving its published URL. It does not write or sign anything.
func BuildManifest(spec Spec) (*Manifest, error) {
	if spec.Channel == "" {
		return nil, fmt.Errorf("release: spec missing channel")
	}
	if spec.Base == "" {
		return nil, fmt.Errorf("release: spec missing base URL")
	}
	base := strings.TrimRight(spec.Base, "/")

	m := &Manifest{Channel: spec.Channel, Components: map[string]Component{}}
	for name, sc := range spec.Components {
		if sc.Version == "" {
			return nil, fmt.Errorf("release: component %q missing version", name)
		}
		c := Component{
			Version:   sc.Version,
			MinCLI:    sc.MinCLI,
			Proto:     sc.Proto,
			KernelVer: sc.KernelVer,
		}
		remoteDir := base + "/" + name + "/" + sc.Version
		if name == ComponentDesktop {
			remoteDir = base + "/desktop"
		}

		switch {
		case sc.File != "":
			a, err := assetFor(sc.File, remoteDir)
			if err != nil {
				return nil, fmt.Errorf("release: component %q: %w", name, err)
			}
			c.URL, c.SHA256, c.Size = a.URL, a.SHA256, a.Size
		case len(sc.Platforms) > 0:
			c.Platforms = map[string]Asset{}
			for _, plat := range sortedKeys(sc.Platforms) {
				a, err := assetFor(sc.Platforms[plat], remoteDir)
				if err != nil {
					return nil, fmt.Errorf("release: component %q platform %q: %w", name, plat, err)
				}
				c.Platforms[plat] = a
			}
		case len(sc.Files) > 0:
			c.Files = map[string]Asset{}
			for _, fname := range sortedKeys(sc.Files) {
				a, err := assetFor(sc.Files[fname], remoteDir)
				if err != nil {
					return nil, fmt.Errorf("release: component %q file %q: %w", name, fname, err)
				}
				c.Files[fname] = a
			}
		default:
			return nil, fmt.Errorf("release: component %q has no file/platforms/files", name)
		}
		m.Components[name] = c
	}
	return m, nil
}

// assetFor hashes a local file and builds its Asset with the derived URL.
func assetFor(localPath, remoteDir string) (Asset, error) {
	sum, size, err := hashFile(localPath)
	if err != nil {
		return Asset{}, err
	}
	return Asset{
		URL:    remoteDir + "/" + path.Base(filepath.ToSlash(localPath)),
		SHA256: sum,
		Size:   size,
	}, nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p) //nolint:gosec // CI-controlled staged artifact path
	if err != nil {
		return "", 0, fmt.Errorf("release: open %s: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("release: hash %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func sortedKeys(mm map[string]string) []string {
	ks := make([]string, 0, len(mm))
	for k := range mm {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Marshal renders the manifest as stable, indented JSON suitable for signing.
func (m *Manifest) Marshal() ([]byte, error) {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("release: marshal manifest: %w", err)
	}
	return out, nil
}
