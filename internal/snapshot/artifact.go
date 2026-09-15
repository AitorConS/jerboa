// Package snapshot owns Jerboa's private snapshot store: validated names,
// private directories, quotas, integrity sidecars and the durable operation
// journal used to recover interrupted create/restore lifecycles. It does not
// talk to a hypervisor; the VM manager produces and consumes the artifacts.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// maxManifestBytes mirrors the fork's own manifest bound.
	maxManifestBytes = 1 << 20
	manifestName     = "manifest.json"
)

// Component is one file of a Firecracker/HVF snapshot artifact.
type Component struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is the subset of the fork's manifest.json that Jerboa validates.
// Config and Compatibility are kept raw: the fork remains the authority on
// their full schema and repeats its own checks on load.
type Manifest struct {
	FormatVersion int                  `json:"format_version"`
	Compatibility json.RawMessage      `json:"compatibility"`
	Config        json.RawMessage      `json:"config"`
	Components    map[string]Component `json:"components"`
}

// ManifestNetwork describes the guest-visible network device in a manifest.
type ManifestNetwork struct {
	IfaceID  string `json:"iface_id"`
	GuestMAC string `json:"guest_mac"`
	Backend  string `json:"backend"`
}

// ManifestConfig is the restore-relevant part of the manifest configuration.
type ManifestConfig struct {
	Machine struct {
		CPUs   int `json:"vcpu_count"`
		Memory int `json:"mem_size_mib"`
	} `json:"machine-config"`
	Drives   []json.RawMessage `json:"drives"`
	Networks []ManifestNetwork `json:"network-interfaces"`
}

// Artifact is a verified snapshot artifact.
type Artifact struct {
	Manifest       Manifest
	Config         ManifestConfig
	ManifestSHA256 string
	// SizeBytes counts every component plus the manifest.
	SizeBytes int64
}

// ErrIntegrity classifies artifact permission, shape and checksum failures.
var ErrIntegrity = errors.New("snapshot integrity check failed")

func integrity(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrIntegrity}, args...)...)
}

// VerifyArtifact checks a snapshot directory produced by the fork: a private
// directory owned by this user, containing exactly the manifest and its
// components as private, single-link regular files whose sizes and SHA-256
// digests match. It re-hashes every byte and never follows links.
func VerifyArtifact(dir string) (Artifact, error) {
	var a Artifact
	if err := privateDir(dir); err != nil {
		return a, err
	}
	data, err := readPrivateFile(filepath.Join(dir, manifestName), maxManifestBytes)
	if err != nil {
		return a, err
	}
	sum := sha256.Sum256(data)
	a.ManifestSHA256 = hex.EncodeToString(sum[:])
	a.SizeBytes = int64(len(data))
	if err := json.Unmarshal(data, &a.Manifest); err != nil {
		return a, integrity("parse manifest: %v", err)
	}
	if a.Manifest.FormatVersion != 1 {
		return a, integrity("unsupported snapshot format %d", a.Manifest.FormatVersion)
	}
	if len(a.Manifest.Components) == 0 {
		return a, integrity("manifest lists no components")
	}
	if err := json.Unmarshal(a.Manifest.Config, &a.Config); err != nil {
		return a, integrity("parse manifest config: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return a, integrity("read artifact: %v", err)
	}
	for _, e := range entries {
		if e.Name() == manifestName {
			continue
		}
		if _, ok := a.Manifest.Components[e.Name()]; !ok {
			return a, integrity("unexpected artifact entry %q", e.Name())
		}
	}
	for name, c := range a.Manifest.Components {
		if name != filepath.Base(name) || name == "." || name == ".." || name == manifestName {
			return a, integrity("invalid component name %q", name)
		}
		if c.Size < 0 || len(c.SHA256) != 64 {
			return a, integrity("invalid component %q", name)
		}
		got, size, err := hashPrivateFile(filepath.Join(dir, name))
		if err != nil {
			return a, err
		}
		if size != c.Size || got != c.SHA256 {
			return a, integrity("component %q does not match its manifest size or SHA-256", name)
		}
		a.SizeBytes += size
	}
	return a, nil
}

func owner(info os.FileInfo) (uint32, uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, uint64(st.Nlink), true //nolint:unconvert // Nlink width differs by platform
}

// privateDir requires a real directory (not a link) owned by this user with no
// group or other permissions.
func privateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return integrity("%v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return integrity("%s is not a directory", path)
	}
	uid, _, ok := owner(info)
	if !ok || int(uid) != os.Geteuid() {
		return integrity("%s is not owned by the daemon user", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return integrity("%s must not be accessible by group or others", path)
	}
	return nil
}

func openPrivateFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // path is resolved inside the private store
	if err != nil {
		return nil, integrity("open %s: %v", filepath.Base(path), err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, integrity("stat %s: %v", filepath.Base(path), err)
	}
	uid, links, ok := owner(info)
	if !info.Mode().IsRegular() || !ok || int(uid) != os.Geteuid() || links != 1 || info.Mode().Perm()&0o077 != 0 {
		_ = f.Close()
		return nil, integrity("%s must be a private single-link regular file owned by the daemon user", filepath.Base(path))
	}
	return f, nil
}

func readPrivateFile(path string, limit int64) ([]byte, error) {
	f, err := openPrivateFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, integrity("read %s: %v", filepath.Base(path), err)
	}
	if int64(len(data)) > limit {
		return nil, integrity("%s exceeds %d bytes", filepath.Base(path), limit)
	}
	return data, nil
}

func hashPrivateFile(path string) (string, int64, error) {
	f, err := openPrivateFile(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, integrity("hash %s: %v", filepath.Base(path), err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
