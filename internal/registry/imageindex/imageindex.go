// Package imageindex defines image indexes: one recipe per platform under
// one tag, so a tag pulls the image built for the host it runs on.
//
// An index is content addressed like a recipe, and has one canonical
// encoding, so one set of recipes has one digest whoever composes it.
package imageindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"

	pkg "github.com/AitorConS/jerboa/internal/package"
)

// MediaType identifies an index document.
const MediaType = "application/vnd.jerboa.image.index.v1+json"

// SchemaVersion is the only index schema this package reads and writes.
const SchemaVersion = 1

// MaxPlatforms is the most entries an index holds.
const MaxPlatforms = 8

// DefaultPlatform is the platform whose recipe a tag naming an index points
// to for clients that know nothing of indexes, when the index has it.
const DefaultPlatform = "linux/amd64"

// Entry is one platform's recipe.
type Entry struct {
	Digest   string `json:"digest"`
	Platform string `json:"platform"`
}

// Index lists one recipe per platform, sorted by platform.
type Index struct {
	SchemaVersion int     `json:"schemaVersion"`
	MediaType     string  `json:"mediaType"`
	Manifests     []Entry `json:"manifests"`
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// New builds an index of entries, in any order.
func New(entries []Entry) (*Index, error) {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Platform < sorted[j].Platform })
	ix := &Index{SchemaVersion: SchemaVersion, MediaType: MediaType, Manifests: sorted}
	if err := ix.Validate(); err != nil {
		return nil, err
	}
	return ix, nil
}

// Validate checks the schema, the digests and platforms, and that entries
// are sorted with one per platform.
func (ix *Index) Validate() error {
	if ix.SchemaVersion != SchemaVersion || ix.MediaType != MediaType {
		return fmt.Errorf("index: unsupported schema %d %q", ix.SchemaVersion, ix.MediaType)
	}
	if len(ix.Manifests) == 0 || len(ix.Manifests) > MaxPlatforms {
		return fmt.Errorf("index: lists %d recipes; an index lists 1 to %d", len(ix.Manifests), MaxPlatforms)
	}
	for i, e := range ix.Manifests {
		if !digestRE.MatchString(e.Digest) {
			return fmt.Errorf("index: invalid recipe digest %q", e.Digest)
		}
		if err := pkg.ValidatePlatform(e.Platform); err != nil {
			return fmt.Errorf("index: %w", err)
		}
		if i > 0 && ix.Manifests[i-1].Platform >= e.Platform {
			if ix.Manifests[i-1].Platform == e.Platform {
				return fmt.Errorf("index: two recipes for %s", e.Platform)
			}
			return fmt.Errorf("index: entries are not sorted by platform")
		}
	}
	return nil
}

// Encode returns the canonical encoding: compact JSON, fields in order.
func (ix *Index) Encode() ([]byte, error) {
	data, err := json.Marshal(ix)
	if err != nil {
		return nil, fmt.Errorf("index: encode: %w", err)
	}
	return data, nil
}

// Digest returns the sha256 digest of the canonical encoding, and the encoding.
func (ix *Index) Digest() (string, []byte, error) {
	data, err := ix.Encode()
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), data, nil
}

// Parse decodes and validates an index. Only the canonical encoding is
// accepted, so its digest is the digest of the bytes that were read.
func Parse(data []byte) (*Index, error) {
	var ix Index
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ix); err != nil {
		return nil, fmt.Errorf("index: parse: %w", err)
	}
	if err := ix.Validate(); err != nil {
		return nil, err
	}
	canonical, err := ix.Encode()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, data) {
		return nil, fmt.Errorf("index: not in canonical encoding")
	}
	return &ix, nil
}

// For returns the recipe for platform, if the index has one.
func (ix *Index) For(platform string) (string, bool) {
	for _, e := range ix.Manifests {
		if e.Platform == platform {
			return e.Digest, true
		}
	}
	return "", false
}

// Platforms lists the index's platforms, sorted.
func (ix *Index) Platforms() []string {
	out := make([]string, len(ix.Manifests))
	for i, e := range ix.Manifests {
		out[i] = e.Platform
	}
	return out
}

// Default returns the recipe a registry tags as the index's default: the
// DefaultPlatform one, or the first.
func (ix *Index) Default() string {
	if d, ok := ix.For(DefaultPlatform); ok {
		return d
	}
	return ix.Manifests[0].Digest
}

// HostPlatform is the platform of the images this host runs natively.
func HostPlatform() string {
	return "linux/" + runtime.GOARCH
}

// Choose picks the recipe to pull: the one for platform, or for the host
// when platform is empty. The error names the platforms there are.
func (ix *Index) Choose(platform string) (string, string, error) {
	want := platform
	if want == "" {
		want = HostPlatform()
	}
	if d, ok := ix.For(want); ok {
		return d, want, nil
	}
	have := strings.Join(ix.Platforms(), ", ")
	if platform == "" {
		return "", "", fmt.Errorf("no image for this host's platform %s; there is %s: choose one with --platform", want, have)
	}
	return "", "", fmt.Errorf("no image for %s; there is %s", want, have)
}
