package pkg

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/AitorConS/jerboa/internal/httpclient"
)

// OpsStore manages locally cached ops packages under a root directory.
// Ops packages preserve their native format: package.manifest + sysroot/ +
// top-level binary, stored at root/<namespace>/<name>_<version>/.
type OpsStore struct {
	root string
	mu   sync.RWMutex
}

// NewOpsStore creates an OpsStore rooted at dir, creating it if needed.
func NewOpsStore(dir string) (*OpsStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ops store mkdir %s: %w", dir, err)
	}
	return &OpsStore{root: dir}, nil
}

// PackageDir returns the local directory for an ops package.
func (s *OpsStore) PackageDir(namespace, name, version string) string {
	return filepath.Join(s.root, namespace, name+"_"+version)
}

// IsDownloaded returns true if the ops package archive exists locally.
func (s *OpsStore) IsDownloaded(namespace, name, version string) bool {
	dir := s.PackageDir(namespace, name, version)
	archive := filepath.Join(dir, ArchSlug()+".tar.gz")
	info, err := os.Stat(archive)
	return err == nil && !info.IsDir()
}

// IsExtracted returns true if the ops package has been extracted and its
// main binary is present. Checking the binary guards against partial
// extractions where directory entries were created but the binary was
// removed (e.g. by AV quarantine) after extraction completed.
func (s *OpsStore) IsExtracted(namespace, name, version string) bool {
	dir := s.PackageDir(namespace, name, version)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	hasContent := false
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".tar.gz") && e.Name() != "manifest.json") {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return false
	}
	cfg, err := s.LoadPackageManifest(namespace, name, version)
	if err == nil && cfg.Program != "" {
		binPath := filepath.Join(dir, filepath.Base(cfg.Program))
		if _, statErr := os.Stat(binPath); os.IsNotExist(statErr) {
			return false
		}
	}
	return true
}

// Download fetches an ops package from repo.ops.city and stores it locally.
// The URL is constructed from OpsPackageBaseURL + /<namespace>/<name>/<version>.tar.gz.
func (s *OpsStore) Download(namespace, name, version string, expectedSHA256 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.PackageDir(namespace, name, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ops download mkdir %s: %w", dir, err)
	}

	archSlug := ArchSlug()
	archivePath := filepath.Join(dir, archSlug+".tar.gz")
	if _, err := os.Stat(archivePath); err == nil {
		slog.Info("ops package already downloaded", "namespace", namespace, "name", name, "version", version)
		return nil
	}

	downloadURL := OpsPackageBaseURL + "/" + namespace + "/" + name + "/" + version + ".tar.gz"
	if archSlug == "arm64" {
		downloadURL = strings.ReplaceAll(downloadURL, ".tar.gz", "/arm64.tar.gz")
	}

	slog.Info("downloading ops package", "namespace", namespace, "name", name, "version", version)

	req, err := http.NewRequest(http.MethodGet, downloadURL, nil) //nolint:noctx // callers don't thread ctx yet
	if err != nil {
		return fmt.Errorf("ops download request: %w", err)
	}

	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return fmt.Errorf("ops download %s/%s: %w", namespace, name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ops download %s/%s: HTTP %d", namespace, name, resp.StatusCode)
	}

	f, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("ops download create %s: %w", archivePath, err)
	}

	hash := sha256.New()
	mw := io.MultiWriter(f, hash)

	size, err := io.Copy(mw, resp.Body)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(archivePath)
		return fmt.Errorf("ops download write: %w", err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(archivePath)
		return fmt.Errorf("ops download close: %w", err)
	}

	if expectedSHA256 != "" {
		got := hex.EncodeToString(hash.Sum(nil))
		if !strings.EqualFold(got, expectedSHA256) {
			_ = os.Remove(archivePath)
			return fmt.Errorf("ops download: sha256 mismatch (got %s, want %s)", got, expectedSHA256)
		}
	}

	slog.Info("ops package downloaded", "namespace", namespace, "name", name, "version", version,
		"size_mb", fmt.Sprintf("%.1f", float64(size)/(1<<20)))

	return nil
}

// Extract decompresses the ops package archive into its directory.
// Ops packages may contain:
//   - package.manifest (JSON config for ops)
//   - A top-level binary (the program itself)
//   - sysroot/ (shared libraries and filesystem layout)
//
// Symlinks are handled on Linux; silently skipped on other platforms.
func (s *OpsStore) Extract(namespace, name, version string) error {
	if s.IsExtracted(namespace, name, version) {
		return nil
	}

	dir := s.PackageDir(namespace, name, version)
	archivePath := filepath.Join(dir, ArchSlug()+".tar.gz")

	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("ops extract open %s: %w", archivePath, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("ops extract gzip %s: %w", name, err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var stripPrefix string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("ops extract tar %s: %w", name, err)
		}

		if strings.HasPrefix(filepath.Base(hdr.Name), "._") {
			continue
		}

		entryName := hdr.Name
		if stripPrefix == "" && (hdr.Typeflag == tar.TypeDir) && strings.Contains(hdr.Name, "/") {
			stripPrefix = hdr.Name
		}
		if stripPrefix != "" {
			entryName = strings.TrimPrefix(hdr.Name, stripPrefix)
		}
		if entryName == "" || entryName == "/" {
			continue
		}

		target := filepath.Join(dir, entryName)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("ops extract mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("ops extract mkdir %s: %w", filepath.Dir(target), err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode)|0o200)
			if err != nil {
				return fmt.Errorf("ops extract create %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return fmt.Errorf("ops extract write %s: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("ops extract close %s: %w", target, err)
			}
		case tar.TypeSymlink:
			if runtime.GOOS == "linux" {
				if err := os.Symlink(hdr.Linkname, target); err != nil {
					slog.Warn("ops extract: symlink failed", "target", target, "error", err)
				}
			}
			// Symlinks are silently skipped on non-Linux hosts; the unikernel
			// builder resolves them at image assembly time on the Linux side.
		}
	}
	return nil
}

// File represents a file to be included in a unikernel image, with both
// its host path (on the build machine) and its guest path (inside the image).
type File struct {
	HostPath  string
	GuestPath string
}

// opsRuntimeBloatDir reports whether a slash-separated package-relative path
// is a directory that is only needed at compile time, not at runtime inside
// a unikernel. Returning true causes the walker to skip the whole subtree.
//
// Excluded subtrees (with typical sizes for eyberg/python3:3.12.3):
//   - lib/python*/config-*/   — 27 MB: static libpython + build headers
//   - lib/python*/test/       —  1.3 MB: CPython's own test suite
//   - lib/python*/lib2to3/    —  1 MB: Python 2→3 migration tool
//   - lib/python*/pydoc_data/ —  1.3 MB: HTML documentation data for pydoc
func opsRuntimeBloatDir(rel string) bool {
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if !strings.HasPrefix(part, "python") {
			continue
		}
		if i+1 >= len(parts) {
			break
		}
		next := parts[i+1]
		if strings.HasPrefix(next, "config-") ||
			next == "test" ||
			next == "lib2to3" ||
			next == "pydoc_data" {
			return true
		}
	}
	return false
}

// ExtractedFiles returns the files inside an extracted ops package as File
// entries with proper guest paths. Files inside sysroot/ get their sysroot-
// relative path as the guest path; top-level files use their basename.
// Build-only subtrees (static libs, test suites, etc.) are excluded to keep
// the image lean — see opsRuntimeBloatDir for the full list.
func (s *OpsStore) ExtractedFiles(namespace, name, version string) ([]File, error) {
	dir := s.PackageDir(namespace, name, version)
	var files []File

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return fmt.Errorf("ops list files rel path: %w", relErr)
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if opsRuntimeBloatDir(rel) {
				return fs.SkipDir
			}
			return nil
		}

		if strings.HasSuffix(rel, ".tar.gz") || rel == "manifest.json" || rel == "package.manifest" {
			return nil
		}
		if strings.HasPrefix(filepath.Base(rel), "._") {
			return nil
		}

		var guestPath string
		if strings.HasPrefix(rel, "sysroot/") {
			guestPath = strings.TrimPrefix(rel, "sysroot/")
		} else {
			guestPath = filepath.Base(rel)
		}

		files = append(files, File{
			HostPath:  path,
			GuestPath: guestPath,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ops list files %s/%s %s: %w", namespace, name, version, err)
	}
	return files, nil
}

// List returns all locally cached ops packages.
func (s *OpsStore) List() ([]OpsPackage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []OpsPackage
	nsEntries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("ops list: %w", err)
	}
	for _, nsEntry := range nsEntries {
		if !nsEntry.IsDir() {
			continue
		}
		namespace := nsEntry.Name()
		pkgEntries, err := os.ReadDir(filepath.Join(s.root, namespace))
		if err != nil {
			continue
		}
		for _, pkgEntry := range pkgEntries {
			if !pkgEntry.IsDir() {
				continue
			}
			pkgName, pkgVersion := splitOpsDirName(pkgEntry.Name())
			result = append(result, OpsPackage{
				Name:      pkgName,
				Version:   pkgVersion,
				Namespace: namespace,
			})
		}
	}
	return result, nil
}

// Remove deletes a locally cached ops package.
func (s *OpsStore) Remove(namespace, name, version string) error {
	dir := s.PackageDir(namespace, name, version)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("ops remove %s/%s: %w", namespace, name, err)
	}
	return nil
}

// SaveManifest caches the ops manifest locally.
func (s *OpsStore) SaveManifest(data []byte) error {
	path := filepath.Join(s.root, "manifest.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("ops save manifest %s: %w", path, err)
	}
	return nil
}

// LoadCachedManifest reads the locally cached ops manifest.
func (s *OpsStore) LoadCachedManifest() (*OpsPackageList, error) {
	path := filepath.Join(s.root, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ops cached manifest: %w", err)
	}
	var list OpsPackageList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("ops cached manifest parse: %w", err)
	}
	return &list, nil
}

// ManifestNeedsUpdate checks if the remote manifest has changed by comparing
// Content-Length via HTTP HEAD. Returns true if update is needed or if check
// fails (safe default: refresh on error).
func ManifestNeedsUpdate(localPath string) bool {
	stat, err := os.Stat(localPath)
	if err != nil {
		return true
	}

	resp, err := http.Head(OpsPackageManifestURL) //nolint:noctx // manifest check has no caller context
	if err != nil {
		return true
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return true
	}

	return stat.Size() != resp.ContentLength
}

// FetchManifestCached downloads the ops manifest only if it has changed since
// the last fetch, using Content-Length comparison.
func (s *OpsStore) FetchManifestCached() (*OpsPackageList, error) {
	cachedPath := filepath.Join(s.root, "manifest.json")

	if _, err := os.Stat(cachedPath); err == nil {
		if !ManifestNeedsUpdate(cachedPath) {
			return s.LoadCachedManifest()
		}
	}

	req, err := http.NewRequest(http.MethodGet, OpsPackageManifestURL, nil) //nolint:noctx // callers don't thread ctx yet
	if err != nil {
		return nil, fmt.Errorf("ops manifest request: %w", err)
	}
	resp, err := httpclient.Default.Do(req)
	if err != nil {
		if _, statErr := os.Stat(cachedPath); statErr == nil {
			slog.Warn("ops manifest fetch failed, using cached copy", "error", err)
			return s.LoadCachedManifest()
		}
		return nil, fmt.Errorf("ops manifest fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		if _, statErr := os.Stat(cachedPath); statErr == nil {
			slog.Warn("ops manifest HTTP error, using cached copy", "status", resp.StatusCode)
			return s.LoadCachedManifest()
		}
		return nil, fmt.Errorf("ops manifest: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ops manifest read: %w", err)
	}

	if err := s.SaveManifest(data); err != nil {
		slog.Warn("ops manifest cache write failed", "error", err)
	}

	var list OpsPackageList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("ops manifest parse: %w", err)
	}
	return &list, nil
}

// splitOpsDirName splits a package directory name like "node_v16.5.0"
// into (name, version). If no underscore, returns the full name with empty version.
func splitOpsDirName(dirName string) (string, string) {
	idx := strings.LastIndex(dirName, "_")
	if idx < 0 {
		return dirName, ""
	}
	return dirName[:idx], dirName[idx+1:]
}

// opsPackageStoreDir returns the path for the ops package store.
// Defaults to ~/.uni/packages-ops/.
func opsPackageStoreDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".uni", "packages-ops")
	}
	return filepath.Join(home, ".uni", "packages-ops")
}

// DefaultOpsStore returns an OpsStore at the default path.
func DefaultOpsStore() (*OpsStore, error) {
	return NewOpsStore(opsPackageStoreDir())
}

// OpsPackageManifestConfig represents the package.manifest JSON from an ops package.
type OpsPackageManifestConfig struct {
	Program string            `json:"Program"`
	Args    []string          `json:"Args"`
	Version string            `json:"Version"`
	Env     map[string]string `json:"Env"`
}

// LoadPackageManifest reads and parses the package.manifest from an extracted
// ops package directory.
func (s *OpsStore) LoadPackageManifest(namespace, name, version string) (*OpsPackageManifestConfig, error) {
	dir := s.PackageDir(namespace, name, version)
	manifestPath := filepath.Join(dir, "package.manifest")

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("ops package manifest %s: %w", manifestPath, err)
	}

	var cfg OpsPackageManifestConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("ops package manifest parse: %w", err)
	}
	return &cfg, nil
}

// FindBinary attempts to find the main binary inside an extracted ops package.
// It checks the package.manifest for the Program field, then falls back to
// looking for ELF files at the top level.
func (s *OpsStore) FindBinary(namespace, name, version string) (string, error) {
	dir := s.PackageDir(namespace, name, version)

	cfg, err := s.LoadPackageManifest(namespace, name, version)
	if err == nil && cfg.Program != "" {
		// ops Program field may be "pkg_version/binary" (relative to the ops packages
		// root), so try direct, OS-separator variant, and basename in order.
		for _, candidate := range []string{
			cfg.Program,
			strings.ReplaceAll(cfg.Program, "/", string(filepath.Separator)),
			filepath.Base(cfg.Program),
		} {
			binPath := filepath.Join(dir, candidate)
			if _, statErr := os.Stat(binPath); statErr == nil {
				return binPath, nil
			}
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("ops find binary: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tar.gz") || e.Name() == "manifest.json" || e.Name() == "package.manifest" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if isELFFile(path) {
			return path, nil
		}
	}

	return "", fmt.Errorf("ops find binary: no ELF binary found in %s/%s:%s", namespace, name, version)
}

// isELFFile checks if a file starts with the ELF magic bytes.
func isELFFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	magic := make([]byte, 4)
	if _, err := f.Read(magic); err != nil {
		return false
	}
	return magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F'
}
