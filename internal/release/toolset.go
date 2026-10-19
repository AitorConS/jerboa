package release

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Toolset describes the host tools published for one kernel toolset version:
// an mkfs binary per host platform ("<goos>-<goarch>"). It is published,
// minisign-signed, at <base>/kernel/<version>/toolset.json beside the kernel
// files, and never changes once published.
//
// Images are distributed as recipes that name the toolset version they were
// built with. mkfs output is byte-identical across hosts for one version, so a
// client that runs that version's mkfs assembles the published disk exactly,
// whatever toolset it uses for its own builds.
type Toolset struct {
	Version string           `json:"version"`
	Mkfs    map[string]Asset `json:"mkfs"`
}

var toolsetVersionRE = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// NormalizeToolsetVersion returns version with a leading "v", the form used in
// release URLs, or an error when it is not a semantic version.
func NormalizeToolsetVersion(version string) (string, error) {
	v := strings.TrimSpace(version)
	if !toolsetVersionRE.MatchString(v) {
		return "", fmt.Errorf("release: invalid toolset version %q", version)
	}
	return "v" + strings.TrimPrefix(v, "v"), nil
}

// FetchToolset downloads and verifies the signed toolset document for version.
func (c *Client) FetchToolset(ctx context.Context, version string) (*Toolset, error) {
	v, err := NormalizeToolsetVersion(version)
	if err != nil {
		return nil, err
	}
	url := c.base() + "/kernel/" + v + "/toolset.json"
	data, err := c.get(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("release: fetch toolset %s: %w", v, err)
	}
	sig, err := c.get(ctx, url+".minisig")
	if err != nil {
		return nil, fmt.Errorf("release: fetch toolset %s signature: %w", v, err)
	}
	if err := c.Pub.Verify(data, sig); err != nil {
		return nil, err
	}
	return ParseToolset(data, v)
}

// ParseToolset decodes and validates a toolset document published for version.
func ParseToolset(data []byte, version string) (*Toolset, error) {
	var t Toolset
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("release: parse toolset: %w", err)
	}
	// The signature covers the document, not the URL it came from: a valid
	// toolset for another version must not be accepted under this one.
	if t.Version != version {
		return nil, fmt.Errorf("release: toolset is for %q, want %q", t.Version, version)
	}
	if len(t.Mkfs) == 0 {
		return nil, fmt.Errorf("release: toolset %s has no mkfs", version)
	}
	for platform, a := range t.Mkfs {
		if !a.valid() || a.Size <= 0 {
			return nil, fmt.Errorf("release: toolset %s mkfs %s missing url/sha256/size", version, platform)
		}
	}
	return &t, nil
}

// MkfsAsset returns the mkfs download for a host platform.
func (t *Toolset) MkfsAsset(goos, goarch string) (Asset, error) {
	a, ok := t.Mkfs[goos+"-"+goarch]
	if !ok {
		return Asset{}, fmt.Errorf("release: toolset %s has no mkfs for %s-%s", t.Version, goos, goarch)
	}
	return a, nil
}
