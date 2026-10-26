// Package recipe defines image recipes: how to rebuild an image's disk
// byte-for-byte from content-addressed files and a pinned kernel toolset.
//
// A registry distributes recipes and blobs instead of disks. The client
// fetches the blobs it lacks, runs the recipe's mkfs version and checks the
// disk against the recipe's digest; mkfs is deterministic across hosts, so
// the result is the published disk.
package recipe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/AitorConS/jerboa/internal/diskclone"
	"github.com/AitorConS/jerboa/internal/image"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/AitorConS/jerboa/internal/release"
)

// MediaType identifies a recipe document.
const MediaType = "application/vnd.jerboa.image.recipe.v1+json"

// SchemaVersion is the only recipe schema this package reads and writes.
const SchemaVersion = 1

// blobScheme prefixes a digest standing for a host path in the manifest.
const blobScheme = "blob:"

// Recipe is everything needed to assemble one image disk.
type Recipe struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Platform      string          `json:"platform"`
	Architecture  string          `json:"architecture"`
	Layout        string          `json:"layout,omitempty"`
	Config        image.Config    `json:"config"`
	Packages      []pkg.Reference `json:"packages,omitempty"`
	Toolset       Toolset         `json:"toolset"`
	// Manifest is the mkfs manifest with every host path written as
	// "blob:sha256:<hex>".
	Manifest   string `json:"manifest"`
	Blobs      []Blob `json:"blobs"`
	DiskDigest string `json:"diskDigest"`
	DiskSize   int64  `json:"diskSize"`
}

// Toolset pins the mkfs version and the boot and kernel images it reads.
type Toolset struct {
	Version string `json:"version"`
	Boot    string `json:"boot"`
	Kernel  string `json:"kernel"`
}

// Blob is one content-addressed input.
type Blob struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

var (
	placeholderRE = regexp.MustCompile(`"` + blobScheme + `(sha256:[0-9a-f]{64})"`)
	blobRefRE     = regexp.MustCompile(`^` + blobScheme + `(sha256:[0-9a-f]{64})$`)
)

// Encode returns the canonical encoding whose SHA-256 identifies the recipe.
func (r *Recipe) Encode() ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("recipe: encode: %w", err)
	}
	return data, nil
}

// Digest returns "sha256:<hex>" of the canonical encoding.
func (r *Recipe) Digest() (string, error) {
	data, err := r.Encode()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return cas.Prefix + hex.EncodeToString(sum[:]), nil
}

// Parse decodes and validates a recipe. Only the canonical encoding is
// accepted, so a recipe's digest is the digest of the bytes that were read.
func Parse(data []byte) (*Recipe, error) {
	var r Recipe
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("recipe: parse: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	canonical, err := r.Encode()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, data) {
		return nil, fmt.Errorf("recipe: not in canonical encoding")
	}
	return &r, nil
}

// Validate checks the recipe is complete and self-consistent: every blob the
// manifest or toolset uses is listed exactly once, and nothing else is.
func (r *Recipe) Validate() error {
	if r.SchemaVersion != SchemaVersion || r.MediaType != MediaType {
		return fmt.Errorf("recipe: unsupported schema %d %q", r.SchemaVersion, r.MediaType)
	}
	if err := pkg.ValidatePlatform(r.Platform); err != nil {
		return fmt.Errorf("recipe: %w", err)
	}
	if r.Architecture != strings.TrimPrefix(r.Platform, "linux/") {
		return fmt.Errorf("recipe: architecture %q does not match platform %q", r.Architecture, r.Platform)
	}
	if r.Layout != "" && r.Layout != image.LayoutStandard && r.Layout != image.LayoutCompact {
		return fmt.Errorf("recipe: unknown layout %q", r.Layout)
	}
	if v, err := release.NormalizeToolsetVersion(r.Toolset.Version); err != nil || v != r.Toolset.Version {
		return fmt.Errorf("recipe: invalid toolset version %q", r.Toolset.Version)
	}
	if _, ok := cas.ParseDigest(r.DiskDigest); !ok || r.DiskSize <= 0 {
		return fmt.Errorf("recipe: invalid disk digest or size")
	}
	if err := pkg.ValidateReferences(r.Packages, r.Platform); err != nil {
		return fmt.Errorf("recipe: %w", err)
	}
	listed := map[string]bool{}
	for i, b := range r.Blobs {
		if _, ok := cas.ParseDigest(b.Digest); !ok || b.Size < 0 {
			return fmt.Errorf("recipe: invalid blob %q", b.Digest)
		}
		if i > 0 && r.Blobs[i-1].Digest >= b.Digest {
			return fmt.Errorf("recipe: blobs must be sorted and unique")
		}
		listed[b.Digest] = true
	}
	used := map[string]bool{r.Toolset.Boot: true, r.Toolset.Kernel: true}
	// The manifest is checked as mkfs parses it: every file mkfs would read
	// from the host must be a blob, whatever the spelling of the key.
	hosts, err := image.ManifestHosts(r.Manifest)
	if err != nil {
		return fmt.Errorf("recipe: %w", err)
	}
	for _, h := range hosts {
		m := blobRefRE.FindStringSubmatch(h)
		if m == nil {
			return fmt.Errorf("recipe: manifest names a host path")
		}
		used[m[1]] = true
	}
	// Assemble swaps placeholders by text, so each one must be a host value
	// and nothing else.
	if strings.Count(r.Manifest, `"`+blobScheme) != len(hosts) || len(placeholderRE.FindAllString(r.Manifest, -1)) != len(hosts) {
		return fmt.Errorf("recipe: manifest has a malformed blob reference")
	}
	for d := range used {
		if !listed[d] {
			return fmt.Errorf("recipe: blob %s is used but not listed", d)
		}
	}
	for d := range listed {
		if !used[d] {
			return fmt.Errorf("recipe: blob %s is listed but not used", d)
		}
	}
	return nil
}

// Recorder records builds as recipes in a blob store.
type Recorder struct {
	Store *cas.Store
}

// Record copies every input of a build into the store, then stores and
// returns the digest of the recipe that assembles its disk.
func (rec Recorder) Record(_ context.Context, b image.BuildRecord) (string, error) {
	ts := b.Config.Toolset
	if ts == nil {
		return "", fmt.Errorf("recipe: build has no toolset")
	}
	version, err := release.NormalizeToolsetVersion(ts.Version)
	if err != nil {
		return "", fmt.Errorf("recipe: %w", err)
	}
	sizes := map[string]int64{}
	put := func(path string) (string, error) {
		d, err := rec.Store.PutFile(path)
		if err != nil {
			return "", err //nolint:wrapcheck // cas errors name the file
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("recipe: %w", err)
		}
		sizes[d] = info.Size()
		return d, nil
	}
	digests := map[string]string{}
	for _, f := range b.Files {
		if f.IsDir || f.HostPath == "" {
			continue
		}
		if digests[f.HostPath], err = put(f.HostPath); err != nil {
			return "", err
		}
	}
	boot, err := put(ts.Boot)
	if err != nil {
		return "", err
	}
	kernel, err := put(ts.Kernel)
	if err != nil {
		return "", err
	}
	var missing string
	manifest := b.RenderManifest(func(host string) string {
		d, ok := digests[host]
		if !ok && missing == "" {
			missing = host
		}
		return blobScheme + d
	})
	if missing != "" {
		return "", fmt.Errorf("recipe: manifest names %s, which is not a build input", missing)
	}
	r := &Recipe{
		SchemaVersion: SchemaVersion,
		MediaType:     MediaType,
		Platform:      b.Manifest.Platform,
		Architecture:  b.Manifest.Architecture,
		Layout:        b.Manifest.Layout,
		Config:        b.Manifest.Config,
		Packages:      b.Manifest.Packages,
		Toolset:       Toolset{Version: version, Boot: boot, Kernel: kernel},
		Manifest:      manifest,
		DiskDigest:    b.Manifest.DiskDigest,
		DiskSize:      b.Manifest.DiskSize,
	}
	for d, size := range sizes {
		r.Blobs = append(r.Blobs, Blob{Digest: d, Size: size})
	}
	sort.Slice(r.Blobs, func(i, j int) bool { return r.Blobs[i].Digest < r.Blobs[j].Digest })
	if err := r.Validate(); err != nil {
		return "", err
	}
	data, err := r.Encode()
	if err != nil {
		return "", err
	}
	digest, err := rec.Store.PutBytes(data)
	if err != nil {
		return "", fmt.Errorf("recipe: store: %w", err)
	}
	return digest, nil
}

// Load reads and validates the recipe stored under digest.
func Load(store *cas.Store, digest string) (*Recipe, error) {
	data, err := store.ReadBytes(digest)
	if err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	return Parse(data)
}

// Assemble writes the recipe's disk to out with the given mkfs, from blobs in
// store, and fails unless the result is exactly the recipe's disk.
func Assemble(ctx context.Context, r *Recipe, store *cas.Store, mkfs, out string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	for _, b := range r.Blobs {
		if !store.Has(b.Digest) {
			return fmt.Errorf("recipe: blob %s is not in the store", b.Digest)
		}
	}
	var pathErr error
	manifest := placeholderRE.ReplaceAllStringFunc(r.Manifest, func(m string) string {
		p, err := store.Path(placeholderRE.FindStringSubmatch(m)[1])
		if err == nil && strings.ContainsAny(p, "\"\\") {
			err = fmt.Errorf("recipe: blob store path %q cannot appear in a manifest", p)
		}
		if err != nil && pathErr == nil {
			pathErr = err
		}
		return `"` + p + `"`
	})
	if pathErr != nil {
		return pathErr
	}
	if err := checkAssembledHosts(r, store, manifest); err != nil {
		return err
	}
	boot, _ := store.Path(r.Toolset.Boot)
	kernel, _ := store.Path(r.Toolset.Kernel)
	_ = os.Remove(out)
	cmd := exec.CommandContext(ctx, mkfs, "-b", boot, "-k", kernel, out) //nolint:gosec // mkfs resolved for the recipe's toolset
	cmd.Stdin = strings.NewReader(manifest)
	// An mkfs that knows this variable refuses host files outside the store;
	// older ones ignore it, and the check above already holds.
	cmd.Env = append(os.Environ(), HostRootEnv+"="+store.Root())
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("recipe: mkfs: %w: %s", err, lastLine(output))
	}
	got, err := diskclone.HashFile(out)
	if err != nil {
		return fmt.Errorf("recipe: hash disk: %w", err)
	}
	if got != r.DiskDigest {
		_ = os.Remove(out)
		return fmt.Errorf("recipe: assembled disk %s does not match %s", got, r.DiskDigest)
	}
	return nil
}

// HostRootEnv names the directory mkfs may read host files from.
const HostRootEnv = "JERBOA_MKFS_HOST_ROOT"

// checkAssembledHosts checks the manifest given to mkfs reads only the
// recipe's blobs from the store.
func checkAssembledHosts(r *Recipe, store *cas.Store, manifest string) error {
	allowed := map[string]bool{}
	for _, b := range r.Blobs {
		p, err := store.Path(b.Digest)
		if err != nil {
			return fmt.Errorf("recipe: %w", err)
		}
		allowed[p] = true
	}
	hosts, err := image.ManifestHosts(manifest)
	if err != nil {
		return fmt.Errorf("recipe: %w", err)
	}
	for _, h := range hosts {
		if !allowed[h] {
			return fmt.Errorf("recipe: manifest names a host path")
		}
	}
	return nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}
