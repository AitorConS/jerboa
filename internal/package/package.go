// Package package manages pre-built runtime packages for unikernel images.
//
// A package is a named, versioned collection of files (typically a language
// runtime like Node.js or Python) that can be included in a unikernel image
// during "uni build --pkg". Packages are stored locally in
// ~/.uni/packages/<name>/<version>/ and are downloaded from a remote index.
package pkg

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/httpclient"
)

// IndexURL is the base URL for the package index.
// Can be overridden in tests to point to a local server.
var IndexURL = "https://github.com/AitorConS/UniCli/releases/download/pkg-index/packages.json"

// Package describes a downloadable runtime package.
type Package struct {
	// Name is the package name (e.g. "node", "python", "redis", "nginx").
	Name string `json:"name"`
	// Version is the semantic version (e.g. "20.11.0").
	Version string `json:"version"`
	// Description is a short human-readable summary.
	Description string `json:"description"`
	// Runtime is the runtime family (e.g. "node", "python").
	Runtime string `json:"runtime"`
	// SHA256 is the expected hex-encoded SHA-256 digest of the archive.
	SHA256 string `json:"sha256"`
	// Size is the archive size in bytes.
	Size int64 `json:"size"`
	// URL is the download URL for the package archive.
	URL string `json:"url"`
	// Created is the publication timestamp.
	Created time.Time `json:"created"`
}

// Index is the top-level package index structure.
type Index struct {
	// Packages maps package name to its available versions.
	Packages map[string][]Package `json:"packages"`
}

// Store manages locally cached packages under a root directory.
type Store struct {
	root string
	mu   sync.RWMutex
}

// NewStore creates a Store rooted at dir, creating it if needed.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("package store mkdir %s: %w", dir, err)
	}
	return &Store{root: dir}, nil
}

// PackageDir returns the local directory for a package version.
func (s *Store) PackageDir(name, version string) string {
	return filepath.Join(s.root, name, version)
}

// IsDownloaded returns true if the package archive exists locally.
func (s *Store) IsDownloaded(name, version string) bool {
	dir := s.PackageDir(name, version)
	archive := filepath.Join(dir, "files.tar.gz")
	info, err := os.Stat(archive)
	return err == nil && !info.IsDir()
}

// Download fetches the package archive from its URL and stores it locally.
// Verifies size and SHA-256 digest after download.
func (s *Store) Download(pkg Package) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.PackageDir(pkg.Name, pkg.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("package download mkdir %s: %w", dir, err)
	}

	archivePath := filepath.Join(dir, "files.tar.gz")
	if _, err := os.Stat(archivePath); err == nil {
		slog.Info("package already downloaded", "name", pkg.Name, "version", pkg.Version)
		return nil
	}

	slog.Info("downloading package", "name", pkg.Name, "version", pkg.Version)

	req, err := http.NewRequest(http.MethodGet, pkg.URL, nil)
	if err != nil {
		return fmt.Errorf("package download request: %w", err)
	}

	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return fmt.Errorf("package download %s: %w", pkg.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("package download %s: HTTP %d", pkg.Name, resp.StatusCode)
	}

	f, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("package download create %s: %w", archivePath, err)
	}

	hash := sha256.New()
	mw := io.MultiWriter(f, hash)

	size, err := io.Copy(mw, resp.Body)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(archivePath)
		return fmt.Errorf("package download write: %w", err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(archivePath)
		return fmt.Errorf("package download close: %w", err)
	}

	if pkg.Size > 0 && size != pkg.Size {
		_ = os.Remove(archivePath)
		return fmt.Errorf("package download: size mismatch (got %d, want %d)", size, pkg.Size)
	}

	if pkg.SHA256 != "" {
		got := hex.EncodeToString(hash.Sum(nil))
		if !strings.EqualFold(got, pkg.SHA256) {
			_ = os.Remove(archivePath)
			return fmt.Errorf("package download: sha256 mismatch (got %s, want %s)", got, pkg.SHA256)
		}
	}

	slog.Info("package downloaded", "name", pkg.Name, "version", pkg.Version, "size_mb", fmt.Sprintf("%.1f", float64(size)/(1<<20)))
	return nil
}

// Remove deletes a specific version of a locally cached package.
func (s *Store) Remove(name, version string) error {
	dir := s.PackageDir(name, version)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("package remove %s: %w", name, err)
	}
	return nil
}

// RemoveAll deletes all locally cached versions of a package.
func (s *Store) RemoveAll(name string) error {
	dir := filepath.Join(s.root, name)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("package remove all %s: %w", name, err)
	}
	return nil
}

// Extract decompresses the package archive into a files subdirectory.
// After extraction the individual files can be listed with ExtractedFiles.
func (s *Store) Extract(pkg Package) error {
	dir := s.PackageDir(pkg.Name, pkg.Version)
	archivePath := filepath.Join(dir, "files.tar.gz")
	filesDir := filepath.Join(dir, "files")

	if s.IsExtracted(pkg.Name, pkg.Version) {
		return nil
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("package extract open %s: %w", archivePath, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("package extract gzip %s: %w", pkg.Name, err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("package extract tar %s: %w", pkg.Name, err)
		}

		target := filepath.Join(filesDir, hdr.Name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("package extract mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("package extract mkdir %s: %w", filepath.Dir(target), err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("package extract create %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return fmt.Errorf("package extract write %s: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("package extract close %s: %w", target, err)
			}
		}
	}
	return nil
}

// IsExtracted returns true if the package has been extracted and its files directory is non-empty.
func (s *Store) IsExtracted(name, version string) bool {
	filesDir := filepath.Join(s.PackageDir(name, version), "files")
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// ExtractedFiles returns the absolute paths of all regular files inside the
// package's extracted files directory.
func (s *Store) ExtractedFiles(name, version string) ([]string, error) {
	filesDir := filepath.Join(s.PackageDir(name, version), "files")
	var files []string
	err := filepath.WalkDir(filesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("package list files %s %s: %w", name, version, err)
	}
	return files, nil
}

// List returns all locally cached package versions.
func (s *Store) List() ([]Package, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("package list: %w", err)
	}
	var result []Package
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		verEntries, err := os.ReadDir(filepath.Join(s.root, name))
		if err != nil {
			continue
		}
		for _, ve := range verEntries {
			if !ve.IsDir() {
				continue
			}
			metaPath := filepath.Join(s.root, name, ve.Name(), "meta.json")
			data, err := os.ReadFile(metaPath)
			if err != nil {
				result = append(result, Package{Name: name, Version: ve.Name()})
				continue
			}
			var pkg Package
			if err := json.Unmarshal(data, &pkg); err != nil {
				result = append(result, Package{Name: name, Version: ve.Name()})
				continue
			}
			result = append(result, pkg)
		}
	}
	return result, nil
}

// SaveMeta writes the package metadata to the local cache.
func (s *Store) SaveMeta(pkg Package) error {
	dir := s.PackageDir(pkg.Name, pkg.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("package meta mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return fmt.Errorf("package meta marshal: %w", err)
	}
	metaPath := filepath.Join(dir, "meta.json")
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		return fmt.Errorf("package meta write %s: %w", metaPath, err)
	}
	return nil
}

// FetchIndex downloads and parses the remote package index.
func FetchIndex() (*Index, error) {
	req, err := http.NewRequest(http.MethodGet, IndexURL, nil)
	if err != nil {
		return nil, fmt.Errorf("package index request: %w", err)
	}
	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return nil, fmt.Errorf("package index fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("package index: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("package index read: %w", err)
	}

	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("package index parse: %w", err)
	}
	return &idx, nil
}

// Search returns packages matching the given query string.
// Matches against name, description, and runtime.
func (idx *Index) Search(query string) []Package {
	var result []Package
	lower := strings.ToLower(query)
	for _, versions := range idx.Packages {
		for _, pkg := range versions {
			if strings.Contains(strings.ToLower(pkg.Name), lower) ||
				strings.Contains(strings.ToLower(pkg.Description), lower) ||
				strings.Contains(strings.ToLower(pkg.Runtime), lower) {
				result = append(result, pkg)
				break
			}
		}
	}
	return result
}

// Latest returns the latest version of a package by name, or nil if not found.
func (idx *Index) Latest(name string) *Package {
	versions, ok := idx.Packages[name]
	if !ok || len(versions) == 0 {
		return nil
	}
	return &versions[0]
}

// Create builds a local package archive from the given binary and optional
// extra files. It produces files.tar.gz and meta.json in the package store.
func (s *Store) Create(name, version, binaryPath string, extraFiles []string, description, runtimeName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.PackageDir(name, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pkg create mkdir %s: %w", dir, err)
	}

	archivePath := filepath.Join(dir, "files.tar.gz")
	if _, err := os.Stat(archivePath); err == nil {
		return fmt.Errorf("pkg create: package %s:%s already exists (remove it first)", name, version)
	}

	allFiles := append([]string{binaryPath}, extraFiles...)
	sha, size, err := s.createArchive(archivePath, allFiles)
	if err != nil {
		return fmt.Errorf("pkg create archive: %w", err)
	}

	meta := Package{
		Name:        name,
		Version:     version,
		Description: description,
		Runtime:     runtimeName,
		SHA256:      sha,
		Size:        size,
		Created:     time.Now().UTC(),
	}

	if err := s.writeMeta(dir, meta); err != nil {
		return fmt.Errorf("pkg create meta: %w", err)
	}

	slog.Info("package created", "name", name, "version", version, "sha256", sha, "size", size)
	return nil
}

func (s *Store) createArchive(outPath string, files []string) (string, int64, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return "", 0, fmt.Errorf("create archive file: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = f.Close()
			_ = os.Remove(outPath)
		}
	}()

	hash := sha256.New()
	mw := io.MultiWriter(f, hash)
	gw := gzip.NewWriter(mw)
	tw := tar.NewWriter(gw)

	for _, path := range files {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return "", 0, fmt.Errorf("stat %s: %w", path, statErr)
		}

		hdr, hdrErr := tar.FileInfoHeader(info, "")
		if hdrErr != nil {
			return "", 0, fmt.Errorf("tar header %s: %w", path, hdrErr)
		}
		hdr.Name = filepath.Base(path)

		if writeErr := tw.WriteHeader(hdr); writeErr != nil {
			return "", 0, fmt.Errorf("tar write header %s: %w", path, writeErr)
		}

		if !info.IsDir() {
			src, openErr := os.Open(path)
			if openErr != nil {
				return "", 0, fmt.Errorf("open %s: %w", path, openErr)
			}
			if _, copyErr := io.Copy(tw, src); copyErr != nil {
				_ = src.Close()
				return "", 0, fmt.Errorf("tar copy %s: %w", path, copyErr)
			}
			_ = src.Close()
		}
	}

	if closeErr := tw.Close(); closeErr != nil {
		return "", 0, fmt.Errorf("tar close: %w", closeErr)
	}
	if gwErr := gw.Close(); gwErr != nil {
		return "", 0, fmt.Errorf("gzip close: %w", gwErr)
	}

	size, seekErr := f.Seek(0, io.SeekEnd)
	if seekErr != nil {
		return "", 0, fmt.Errorf("archive seek: %w", seekErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		return "", 0, fmt.Errorf("archive close: %w", closeErr)
	}
	cleanup = false

	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func (s *Store) writeMeta(dir string, meta Package) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("meta marshal: %w", err)
	}
	metaPath := filepath.Join(dir, "meta.json")
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		return fmt.Errorf("meta write %s: %w", metaPath, err)
	}
	return nil
}
