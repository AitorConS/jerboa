// Package package manages pre-built runtime packages for unikernel images.
//
// A package is a named, versioned collection of files (typically a language
// runtime like Node.js or Python) that can be included in a unikernel image
// during "jerboa build --pkg". Packages are stored locally in
// ~/.jerboa/packages/<name>/<version>/ and are downloaded from a remote index.
package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AitorConS/jerboa/internal/httpclient"
	"github.com/AitorConS/jerboa/internal/naming"
)

// validatePackageRef rejects package coordinates that could escape the store
// root. Name and version originate from a remote index (see IndexURL) and are
// joined onto on-disk paths, so an entry like {"name": "../../../.ssh"} must be
// refused before any filesystem operation reads, writes, or deletes through it.
func validatePackageRef(name, version string) error {
	if err := naming.ValidateResourceName("package", name); err != nil {
		return err
	}
	if err := naming.ValidateResourceName("package version", version); err != nil {
		return err
	}
	return nil
}

// IndexURL is the base URL for the package index.
// Can be overridden in tests to point to a local server.
var IndexURL = "https://github.com/AitorConS/jerboa/releases/download/pkg-index/packages.json"

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
	if err := validatePackageRef(name, version); err != nil {
		return false
	}
	dir := s.PackageDir(name, version)
	archive := filepath.Join(dir, "files.tar.gz")
	info, err := os.Stat(archive)
	return err == nil && !info.IsDir()
}

// Download fetches the package archive from its URL and stores it locally.
// Verifies size and SHA-256 digest after download.
func (s *Store) Download(pkg Package) error {
	if err := validatePackageRef(pkg.Name, pkg.Version); err != nil {
		return err
	}

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

	// A package archive can be hundreds of MiB (e.g. a language runtime), so it
	// is streamed with no total deadline; the stall watchdog below aborts only a
	// dead connection. The cancel unblocks a Read stuck on a silent socket.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pkg.URL, nil)
	if err != nil {
		return fmt.Errorf("package download request: %w", err)
	}

	resp, err := httpclient.Streaming.Do(req)
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

	size, err := httpclient.CopyWithStall(mw, resp.Body, httpclient.DefaultStallTimeout, cancel)
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
		return fmt.Errorf("package download: %w (got %d, want %d)", ErrSizeMismatch, size, pkg.Size)
	}

	if pkg.SHA256 != "" {
		got := hex.EncodeToString(hash.Sum(nil))
		if !strings.EqualFold(got, pkg.SHA256) {
			_ = os.Remove(archivePath)
			return fmt.Errorf("package download: %w (got %s, want %s)", ErrChecksumMismatch, got, pkg.SHA256)
		}
	}

	slog.Info("package downloaded", "name", pkg.Name, "version", pkg.Version, "size_mb", fmt.Sprintf("%.1f", float64(size)/(1<<20)))
	return nil
}

// Remove deletes a specific version of a locally cached package.
func (s *Store) Remove(name, version string) error {
	if err := validatePackageRef(name, version); err != nil {
		return err
	}
	dir := s.PackageDir(name, version)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("package %s:%s not found locally", name, version)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("package remove %s: %w", name, err)
	}
	return nil
}

// RemoveAll deletes all locally cached versions of a package.
func (s *Store) RemoveAll(name string) error {
	if err := naming.ValidateResourceName("package", name); err != nil {
		return err
	}
	dir := filepath.Join(s.root, name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("package %s not found locally", name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("package remove all %s: %w", name, err)
	}
	return nil
}

// Extract decompresses the package archive into a files subdirectory.
// After extraction the individual files can be listed with ExtractedFiles.
func (s *Store) Extract(pkg Package) (err error) {
	if err := validatePackageRef(pkg.Name, pkg.Version); err != nil {
		return err
	}
	dir := s.PackageDir(pkg.Name, pkg.Version)
	archivePath := filepath.Join(dir, "files.tar.gz")
	filesDir := filepath.Join(dir, "files")

	if s.IsExtracted(pkg.Name, pkg.Version) {
		return nil
	}

	// A failed extraction must not leave a half-written tree behind:
	// IsExtracted only checks that files/ is non-empty, so the next call would
	// report the truncated -- or attacker-shaped -- result as complete.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(filesDir)
		}
	}()

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
	budget := maxExtractedBytes
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("package extract tar %s: %w", pkg.Name, err)
		}

		// tar -C dir . prepends a "." / "./" root entry that maps to filesDir
		// itself; it needs no extraction and would otherwise trip the containment
		// guard below, so skip it while still validating every child entry.
		if filepath.Clean(filepath.FromSlash(hdr.Name)) == "." {
			continue
		}

		// Defend against archive path traversal ("Zip Slip"). safePath cleans
		// and validates the entry name, rejecting entries that would escape
		// filesDir via ".." or other path tricks.
		if !safePath(filesDir, hdr.Name) {
			return fmt.Errorf("package extract: path %q escapes extraction directory", hdr.Name)
		}
		// Canonical zip-slip guard: filepath.Join cleans the entry name (resolving
		// any ".."), then HasPrefix confirms the result stays within filesDir.
		// safePath enforces the same invariant, but taint trackers only credit a
		// HasPrefix barrier computed in the function that performs the write.
		target := filepath.Join(filesDir, hdr.Name)
		if !strings.HasPrefix(target, filepath.Clean(filesDir)+string(os.PathSeparator)) {
			return fmt.Errorf("package extract: path %q escapes extraction directory", hdr.Name)
		}
		// A lexically safe path can still resolve outside filesDir if an earlier
		// entry planted a symlink along the way, so check the real chain.
		if traversesSymlink(filesDir, target) {
			return fmt.Errorf("package extract: path %q resolves through a symlink", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			// Force owner rwx: a 0o555 (or 0o000) directory entry would
			// otherwise make every later write beneath it fail with EACCES.
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode).Perm()|0o700); err != nil {
				return fmt.Errorf("package extract mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("package extract mkdir %s: %w", filepath.Dir(target), err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode).Perm())
			if err != nil {
				return fmt.Errorf("package extract create %s: %w", target, err)
			}
			n, err := io.Copy(out, io.LimitReader(tr, budget+1))
			budget -= n
			if err != nil {
				_ = out.Close()
				return fmt.Errorf("package extract write %s: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("package extract close %s: %w", target, err)
			}
			if budget < 0 {
				return fmt.Errorf("package extract %s: archive expands beyond %d bytes", pkg.Name, maxExtractedBytes)
			}
		}
	}
	return nil
}

// IsExtracted returns true if the package has been extracted and its files directory is non-empty.
func (s *Store) IsExtracted(name, version string) bool {
	if err := validatePackageRef(name, version); err != nil {
		return false
	}
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
	if err := validatePackageRef(name, version); err != nil {
		return nil, err
	}
	filesDir := filepath.Join(s.PackageDir(name, version), "files")
	var files []string
	err := filepath.WalkDir(filesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("package list files %s %s: %w", name, version, err)
	}
	return files, nil
}

// ExtractedFileList returns the package's extracted files as File entries whose
// GuestPath is the file's destination inside the image, derived from its path
// under the extraction root. A legacy flat archive (every file at the root)
// yields basename guest paths — unchanged from before; a structured archive
// (from CreateFromFiles / from-docker) yields the preserved absolute layout so
// interpreters and libraries land where the ELF references them. This is the
// jerboa-store analog of OpsStore.ExtractedFiles.
func (s *Store) ExtractedFileList(name, version string) ([]File, error) {
	if err := validatePackageRef(name, version); err != nil {
		return nil, err
	}
	filesDir := filepath.Join(s.PackageDir(name, version), "files")
	var files []File
	err := filepath.WalkDir(filesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(filesDir, p)
		if relErr != nil {
			return fmt.Errorf("package rel path: %w", relErr)
		}
		files = append(files, File{HostPath: p, GuestPath: filepath.ToSlash(rel)})
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
	if err := validatePackageRef(pkg.Name, pkg.Version); err != nil {
		return err
	}
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
	idx, _, err := fetchIndexRaw()
	return idx, err
}

// fetchIndexRaw downloads the remote package index, returning both the parsed
// index and the raw JSON bytes (for cache writes).
func fetchIndexRaw() (*Index, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, IndexURL, nil) //nolint:noctx // callers don't thread ctx yet
	if err != nil {
		return nil, nil, fmt.Errorf("package index request: %w", err)
	}
	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("package index fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("package index: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("package index read: %w", err)
	}

	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, nil, fmt.Errorf("package index parse: %w", err)
	}
	return &idx, data, nil
}

// FetchIndexCached returns the package index without making builds depend on
// the network being reachable: a successful remote fetch is cached on disk at
// <root>/index.json; when the fetch fails, the cached copy is used, and as a
// last resort an index is synthesized from locally downloaded package
// metadata (meta.json written by SaveMeta). Only when all three sources are
// unavailable does it return the fetch error.
func (s *Store) FetchIndexCached() (*Index, error) {
	cachePath := filepath.Join(s.root, "index.json")

	idx, raw, fetchErr := fetchIndexRaw()
	if fetchErr == nil {
		// Best-effort cache write: a failure only degrades offline behavior.
		_ = os.WriteFile(cachePath, raw, 0o644)
		return idx, nil
	}

	if data, err := os.ReadFile(cachePath); err == nil {
		var cached Index
		if err := json.Unmarshal(data, &cached); err == nil {
			slog.Warn("package index unreachable; using cached copy", "cache", cachePath, "err", fetchErr)
			return &cached, nil
		}
	}

	local, err := s.List()
	if err != nil || len(local) == 0 {
		return nil, fetchErr
	}
	synth := &Index{Packages: make(map[string][]Package, len(local))}
	for _, p := range local {
		synth.Packages[p.Name] = append(synth.Packages[p.Name], p)
	}
	slog.Warn("package index unreachable; resolving from locally downloaded packages", "err", fetchErr)
	return synth, nil
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
// extra files, rooting every file at its basename in the image. It produces
// files.tar.gz and meta.json in the package store. For files that must land at a
// specific absolute path in the image (a Docker binary's interpreter and shared
// libraries), use CreateFromFiles instead.
func (s *Store) Create(name, version, binaryPath string, extraFiles []string, description, runtimeName string) error {
	files := make([]File, 0, 1+len(extraFiles))
	files = append(files, File{HostPath: binaryPath, GuestPath: filepath.Base(binaryPath)})
	for _, p := range extraFiles {
		files = append(files, File{HostPath: p, GuestPath: filepath.Base(p)})
	}
	return s.CreateFromFiles(name, version, files, description, runtimeName)
}

// CreateFromFiles builds a local package archive from files whose GuestPath is
// the destination path inside the image (leading slash optional). It preserves
// the directory layout instead of flattening to basenames, so a dynamically
// linked binary's interpreter (/lib64/ld-linux-…) and libraries (/lib/…) land
// where the ELF references them — without this a from-docker package fails
// preflight for essentially every real Docker image. A
// File with IsDir set creates an empty directory in the image.
func (s *Store) CreateFromFiles(name, version string, files []File, description, runtimeName string) error {
	if err := validatePackageRef(name, version); err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("pkg create: no files to package")
	}

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

	sha, size, err := createArchive(archivePath, files)
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

// tarEntryName normalizes a guest path to a clean, forward-slashed, relative tar
// entry name: no leading slash and no "." / ".." components (which would let a
// crafted package escape the extraction root). It returns "" for a path that
// normalizes to nothing.
func tarEntryName(guest string) string {
	g := path.Clean("/" + filepath.ToSlash(guest))
	g = strings.TrimPrefix(g, "/")
	if g == "" || g == "." {
		return ""
	}
	return g
}

func createArchive(outPath string, files []File) (string, int64, error) {
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

	for _, file := range files {
		entryName := tarEntryName(file.GuestPath)
		if entryName == "" {
			return "", 0, fmt.Errorf("pkg create: file has an empty guest path (host %q)", file.HostPath)
		}

		if file.IsDir {
			if writeErr := tw.WriteHeader(&tar.Header{
				Name:     entryName + "/",
				Typeflag: tar.TypeDir,
				Mode:     0o755,
			}); writeErr != nil {
				return "", 0, fmt.Errorf("tar write dir header %s: %w", entryName, writeErr)
			}
			continue
		}

		info, statErr := os.Stat(file.HostPath)
		if statErr != nil {
			return "", 0, fmt.Errorf("stat %s: %w", file.HostPath, statErr)
		}

		hdr, hdrErr := tar.FileInfoHeader(info, "")
		if hdrErr != nil {
			return "", 0, fmt.Errorf("tar header %s: %w", file.HostPath, hdrErr)
		}
		hdr.Name = entryName

		if writeErr := tw.WriteHeader(hdr); writeErr != nil {
			return "", 0, fmt.Errorf("tar write header %s: %w", entryName, writeErr)
		}

		if !info.IsDir() {
			src, openErr := os.Open(file.HostPath)
			if openErr != nil {
				return "", 0, fmt.Errorf("open %s: %w", file.HostPath, openErr)
			}
			if _, copyErr := io.Copy(tw, src); copyErr != nil {
				_ = src.Close()
				return "", 0, fmt.Errorf("tar copy %s: %w", file.HostPath, copyErr)
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

// Push uploads a local package archive and metadata to a remote package index.
// The index must accept POST /packages with multipart form data (archive + metadata).
func (s *Store) Push(name, version string, indexURL string) error {
	if err := validatePackageRef(name, version); err != nil {
		return err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	dir := s.PackageDir(name, version)

	metaPath := filepath.Join(dir, "meta.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("pkg push: read meta: %w", err)
	}
	var meta Package
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return fmt.Errorf("pkg push: parse meta: %w", err)
	}

	archivePath := filepath.Join(dir, "files.tar.gz")
	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		return fmt.Errorf("pkg push: read archive: %w", err)
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("pkg push: marshal meta: %w", err)
	}

	body, contentType, err := pushMultipart(metaJSON, archiveData)
	if err != nil {
		return fmt.Errorf("pkg push: build multipart: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, indexURL+"/packages", body) //nolint:noctx // callers don't thread ctx yet
	if err != nil {
		return fmt.Errorf("pkg push: request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return fmt.Errorf("pkg push: upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pkg push: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	slog.Info("package pushed", "name", name, "version", version, "url", indexURL)
	return nil
}

// DockerImageConfig is the subset of an OCI image config used to derive a
// package's program automatically: what the container would run (Entrypoint +
// Cmd) and the environment it expects.
type DockerImageConfig struct {
	WorkingDir string   `json:"WorkingDir"`
	Entrypoint []string `json:"Entrypoint"`
	Cmd        []string `json:"Cmd"`
	Env        []string `json:"Env"`
}

// InspectDockerImage reads an image's OCI config via `docker image inspect`,
// pulling the image first when it is not available locally.
func InspectDockerImage(image string) (*DockerImageConfig, error) {
	out, err := dockerInspectConfig(image)
	if err != nil {
		// The image may simply not be pulled yet; docker inspect does not pull.
		if pullErr := exec.Command("docker", "pull", image).Run(); pullErr != nil { //nolint:noctx // interactive CLI call
			return nil, fmt.Errorf("docker inspect %s: %w (docker pull also failed: %w)", image, err, pullErr)
		}
		out, err = dockerInspectConfig(image)
		if err != nil {
			return nil, fmt.Errorf("docker inspect %s: %w", image, err)
		}
	}
	return ParseDockerConfig(out)
}

func dockerInspectConfig(image string) ([]byte, error) {
	out, err := exec.Command("docker", "image", "inspect", "-f", "{{json .Config}}", image).Output() //nolint:noctx // interactive CLI call
	if err != nil {
		return nil, fmt.Errorf("docker image inspect %s: %w", image, err)
	}
	return out, nil
}

// ParseDockerConfig parses the JSON printed by
// `docker image inspect -f '{{json .Config}}'`.
func ParseDockerConfig(data []byte) (*DockerImageConfig, error) {
	var cfg DockerImageConfig
	if err := json.Unmarshal(bytes.TrimSpace(data), &cfg); err != nil {
		return nil, fmt.Errorf("parse docker image config: %w", err)
	}
	return &cfg, nil
}

// ProgramCandidate returns the path of the program the image would run:
// the first Entrypoint element, or the first Cmd element when there is no
// entrypoint. Empty when the image declares neither.
func (c *DockerImageConfig) ProgramCandidate() string {
	if len(c.Entrypoint) > 0 {
		return c.Entrypoint[0]
	}
	if len(c.Cmd) > 0 {
		return c.Cmd[0]
	}
	return ""
}

// ProgramArgs returns the argv[1..] that accompany ProgramCandidate:
// the rest of Entrypoint plus all of Cmd (Docker's ENTRYPOINT+CMD semantics),
// or the rest of Cmd when there is no entrypoint.
func (c *DockerImageConfig) ProgramArgs() []string {
	if len(c.Entrypoint) > 0 {
		return append(append([]string{}, c.Entrypoint[1:]...), c.Cmd...)
	}
	if len(c.Cmd) > 0 {
		return append([]string{}, c.Cmd[1:]...)
	}
	return nil
}

// IsShellLauncher reports whether the program path looks like a shell or a
// shell script — the standard Docker entrypoint pattern that cannot work in a
// single-process unikernel (there is no shell to run it and no exec to hand
// off to the real program).
func IsShellLauncher(program string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(program)))
	if strings.HasSuffix(base, ".sh") {
		return true
	}
	switch base {
	case "sh", "bash", "dash", "ash", "tini", "dumb-init", "docker-entrypoint":
		return true
	}
	return false
}

// ResolveDockerProgramPath resolves a program name from an image's config to an
// absolute path inside the container (e.g. "node" → "/usr/local/bin/node"),
// using the exported filesystem and OCI PATH. Absolute inputs are returned
// as-is.
func ResolveDockerProgramPath(image, program string) (string, error) {
	if strings.HasPrefix(program, "/") {
		return program, nil
	}
	cfg, err := InspectDockerImage(image)
	if err != nil {
		return "", err
	}
	fs, cleanup, err := exportContainerFS(image)
	if err != nil {
		return "", err
	}
	defer cleanup()
	return resolveDockerProgram(fs, cfg, program)
}

func resolveDockerProgram(fs *containerFS, cfg *DockerImageConfig, program string) (string, error) {
	searchPath := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, env := range cfg.Env {
		if strings.HasPrefix(env, "PATH=") {
			searchPath = strings.TrimPrefix(env, "PATH=")
		}
	}
	for _, dir := range strings.Split(searchPath, ":") {
		candidate := path.Join("/", cfg.WorkingDir, dir, program)
		if strings.HasPrefix(dir, "/") {
			candidate = path.Join(dir, program)
		}
		if strings.Contains(program, "/") {
			candidate = path.Join("/", cfg.WorkingDir, program)
		}
		if resolved, err := fs.resolve(candidate); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("program %q not found on container PATH", program)
}

// Ldd analyses a binary with ldd and returns its shared library dependencies as
// resolved against the host filesystem. A non-zero exit (e.g. "not a dynamic
// executable") is returned as an error; symbol-version mismatches do not fail
// ldd (they are warnings on stderr) and are surfaced separately by LibMismatches.
func Ldd(binaryPath string) ([]string, error) {
	cmd := exec.Command("ldd", binaryPath) //nolint:noctx // ldd is a static utility call with no meaningful context
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ldd %s: %w", binaryPath, err)
	}
	return parseLddLibs(string(output)), nil
}

// LddSysroot resolves a binary's shared library dependencies against sysroot
// instead of the host, so libraries are picked from the distro the binary was
// built for rather than whatever happens to be installed on the build machine.
// It runs the sysroot's own dynamic linker with --library-path pointed at the
// sysroot lib dirs, and returns the resolved sysroot paths plus the interpreter
// (the binary cannot boot without its loader). Libraries the loader resolved
// from outside the sysroot (its built-in default search paths still hit the
// host) are dropped, so a sysroot build never silently bundles host libraries.
// Only existing files are returned.
func LddSysroot(binaryPath, sysroot string) ([]string, error) {
	loader, err := findLoader(sysroot, binaryPath)
	if err != nil {
		return nil, err
	}
	out, err := runLoaderList(loader, sysrootLibDirs(sysroot), binaryPath)
	if err != nil {
		return nil, err
	}

	libs := parseLddLibs(out)
	// The interpreter itself may not be listed by --list; bundle it explicitly so
	// the guest has the exact loader the binary references.
	libs = append(libs, loader)

	seen := make(map[string]struct{}, len(libs))
	var existing []string
	for _, lib := range libs {
		if _, dup := seen[lib]; dup {
			continue
		}
		seen[lib] = struct{}{}
		// The loader's default search path can resolve a dependency from the host
		// even with --library-path set; anything outside the sysroot is not part
		// of this build and must not be bundled.
		if !isUnderDir(sysroot, lib) {
			continue
		}
		if _, statErr := os.Stat(lib); statErr == nil {
			existing = append(existing, lib)
		}
	}
	if len(existing) == 0 {
		return nil, fmt.Errorf("no libraries resolved under sysroot %q for %s", sysroot, binaryPath)
	}
	return existing, nil
}

// LibMismatches runs the loader (host ldd, or the sysroot linker when sysroot is
// non-empty) and returns the lines reporting dependencies that will not satisfy
// the binary at runtime: unresolved or version-mismatched deps ("=> not found",
// "version `X' not found"), plus — for a sysroot build — any dependency the
// loader resolved from outside the sysroot. An empty slice means every
// dependency resolved cleanly. A nil error with an empty slice is only returned
// when the loader actually ran.
func LibMismatches(binaryPath, sysroot string) ([]string, error) {
	var out string
	if sysroot != "" {
		loader, err := findLoader(sysroot, binaryPath)
		if err != nil {
			return nil, err
		}
		o, err := runLoaderList(loader, sysrootLibDirs(sysroot), binaryPath)
		if err != nil {
			return nil, err
		}
		out = o
	} else {
		cmd := exec.Command("ldd", binaryPath) //nolint:noctx // static utility call
		o, err := cmd.CombinedOutput()
		// ldd exits non-zero when a dependency is unresolved but still prints the
		// diagnostic lines; a non-ExitError means it could not run at all.
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return nil, fmt.Errorf("ldd %s: %w", binaryPath, err)
		}
		out = string(o)
	}

	var problems []string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		// "libfoo => not found" and "libbar.so: version `X' not found" both
		// contain "not found"; that single marker catches every mismatch class.
		if strings.Contains(l, "not found") {
			problems = append(problems, l)
		}
	}
	if sysroot != "" {
		for _, lib := range parseLddLibs(out) {
			if !isUnderDir(sysroot, lib) {
				problems = append(problems, "resolved outside sysroot: "+lib)
			}
		}
	}
	return problems, nil
}

// runLoaderList runs "<loader> --library-path <dirs> --list <binary>" and returns
// its combined output. The loader exits non-zero when a dependency is unresolved
// yet still lists what it found, so an ExitError is tolerated; any other error
// (the loader could not be executed) is returned so callers do not mistake a
// failed inspection for a clean one.
func runLoaderList(loader string, libDirs []string, binaryPath string) (string, error) {
	cmd := exec.Command(loader, "--library-path", strings.Join(libDirs, ":"), "--list", binaryPath) //nolint:noctx,gosec // daemon-side call; loader/binary are operator-supplied package inputs
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return "", fmt.Errorf("run loader %s: %w", loader, err)
	}
	return string(out), nil
}

// isUnderDir reports whether path is dir itself or nested inside it, after
// cleaning both. It guards against the loader resolving a dependency from a host
// path outside the intended sysroot.
func isUnderDir(dir, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// parseLddLibs extracts resolved library paths from ldd / "ld.so --list" output.
// Lines take the form "libfoo.so => /path (0xaddr)" or a bare "/path (0xaddr)"
// for the interpreter; the linux-vdso pseudo-library and unresolved "not found"
// entries are skipped.
func parseLddLibs(output string) []string {
	var libs []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "linux-vdso") {
			continue
		}
		if strings.Contains(line, "=>") {
			parts := strings.SplitN(line, "=>", 2)
			if len(parts) == 2 {
				path := strings.TrimSpace(parts[1])
				path = trimLddAddress(path)
				path = strings.TrimSpace(path)
				if path != "" && path != "not found" {
					libs = append(libs, path)
				}
			}
		} else if strings.HasPrefix(line, "/") {
			// Bare interpreter line: "/path/ld-linux... (0xaddr)". Strip only the
			// trailing address annotation so paths containing spaces survive.
			path := strings.TrimSpace(trimLddAddress(line))
			if path != "" {
				libs = append(libs, path)
			}
		}
	}
	return libs
}

// findLoader locates the dynamic linker/interpreter inside sysroot for binaryPath.
// It first honors the exact interpreter the binary requests (its ELF PT_INTERP),
// mapped into the sysroot, so multi-arch or nonstandard sysroots get the right
// loader. It falls back to the conventional glibc loader locations only when the
// binary's interpreter cannot be determined or is absent from the sysroot.
func findLoader(sysroot, binaryPath string) (string, error) {
	if interp, err := elfInterp(binaryPath); err == nil && interp != "" {
		p := filepath.Join(sysroot, interp)
		if _, statErr := os.Stat(p); statErr == nil {
			return p, nil
		}
	}

	candidates := []string{
		"lib64/ld-linux-x86-64.so.2",
		"lib/x86_64-linux-gnu/ld-linux-x86-64.so.2",
		"lib/ld-linux-x86-64.so.2",
		"lib/ld-linux-aarch64.so.1",
		"lib/aarch64-linux-gnu/ld-linux-aarch64.so.1",
	}
	for _, c := range candidates {
		p := filepath.Join(sysroot, c)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	// Fall back to a glob for any ld-linux*.so* under sysroot's lib dirs.
	matches, _ := filepath.Glob(filepath.Join(sysroot, "lib*", "ld-linux*.so*"))
	if len(matches) > 0 {
		return matches[0], nil
	}
	matches, _ = filepath.Glob(filepath.Join(sysroot, "lib", "*", "ld-linux*.so*"))
	if len(matches) > 0 {
		return matches[0], nil
	}
	return "", fmt.Errorf("no dynamic linker found under sysroot %q", sysroot)
}

// elfInterp returns the interpreter (dynamic linker) path requested by an ELF
// binary's PT_INTERP program header, e.g. "/lib64/ld-linux-x86-64.so.2". It
// returns an empty string with a nil error for a static binary (no PT_INTERP).
func elfInterp(binaryPath string) (string, error) {
	f, err := elf.Open(binaryPath)
	if err != nil {
		return "", fmt.Errorf("open elf %s: %w", binaryPath, err)
	}
	defer func() { _ = f.Close() }()

	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		data := make([]byte, p.Filesz)
		if _, err := p.ReadAt(data, 0); err != nil {
			return "", fmt.Errorf("read PT_INTERP %s: %w", binaryPath, err)
		}
		return strings.TrimRight(string(data), "\x00"), nil
	}
	return "", nil
}

// sysrootLibDirs returns the conventional shared-library directories under
// sysroot that exist, for use as the loader's --library-path.
func sysrootLibDirs(sysroot string) []string {
	rel := []string{
		"lib",
		"lib64",
		"usr/lib",
		"usr/lib64",
		"lib/x86_64-linux-gnu",
		"usr/lib/x86_64-linux-gnu",
		"lib/aarch64-linux-gnu",
		"usr/lib/aarch64-linux-gnu",
	}
	var dirs []string
	for _, r := range rel {
		p := filepath.Join(sysroot, r)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			dirs = append(dirs, p)
		}
	}
	return dirs
}

// MissingFiles analyses a binary with ldd and returns library paths that are
// not present on the local filesystem. Useful for identifying which shared
// libraries need to be bundled in a package alongside the binary.
func MissingFiles(binaryPath string) ([]string, error) {
	libs, err := Ldd(binaryPath)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, lib := range libs {
		if _, err := os.Stat(lib); err != nil {
			missing = append(missing, lib)
		}
	}
	return missing, nil
}

// trimLddAddress removes the trailing address annotation from ldd output lines,
// e.g. "/lib/x86_64-linux-gnu/libc.so.6 (0x00007f...)" becomes "/lib/x86_64-linux-gnu/libc.so.6".
func trimLddAddress(s string) string {
	idx := strings.LastIndex(s, " (0x")
	if idx >= 0 {
		return s[:idx]
	}
	return s
}

func pushMultipart(metaJSON, archiveData []byte) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	metaHeader := make(textproto.MIMEHeader)
	metaHeader.Set("Content-Disposition", `form-data; name="metadata"`)
	metaHeader.Set("Content-Type", "application/json")
	metaPart, err := w.CreatePart(metaHeader)
	if err != nil {
		return nil, "", fmt.Errorf("create meta part: %w", err)
	}
	if _, err := metaPart.Write(metaJSON); err != nil {
		return nil, "", fmt.Errorf("write meta: %w", err)
	}

	archivePart, err := w.CreateFormFile("archive", "files.tar.gz")
	if err != nil {
		return nil, "", fmt.Errorf("create archive part: %w", err)
	}
	if _, err := archivePart.Write(archiveData); err != nil {
		return nil, "", fmt.Errorf("write archive: %w", err)
	}

	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart: %w", err)
	}

	return &buf, w.FormDataContentType(), nil
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
