package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// BuildCache stores source hashes to skip redundant builds.
// Cache entries live in <storeDir>/<cacheKey>.json.
type BuildCache struct {
	storeDir string
}

// NewBuildCache creates a BuildCache rooted at dir.
func NewBuildCache(dir string) (*BuildCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("build cache mkdir %s: %w", dir, err)
	}
	return &BuildCache{storeDir: dir}, nil
}

// CacheKey computes a deterministic hash from the source directory and options.
// It walks the directory, hashes every non-ignored file, and combines with the
// language and entrypoint.
func CacheKey(dir string, lang Lang, entrypoint string, extraFiles []string) (string, error) {
	h := sha256.New()

	fmt.Fprintf(h, "lang:%s\n", lang)
	fmt.Fprintf(h, "entrypoint:%s\n", entrypoint)

	ignore, err := LoadIgnoreFile(dir)
	if err != nil {
		return "", fmt.Errorf("cache key: %w", err)
	}

	var paths []string
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return fmt.Errorf("cache key rel path: %w", rerr)
		}
		rel = filepath.ToSlash(rel)
		if ignore.Match(rel, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() {
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("cache key walk: %w", err)
	}

	sort.Strings(paths)

	for _, p := range paths {
		fullPath := filepath.Join(dir, p)
		f, fErr := os.Open(fullPath)
		if fErr != nil {
			return "", fmt.Errorf("cache key open %s: %w", p, fErr)
		}
		fmt.Fprintf(h, "file:%s:", p)
		if _, copyErr := io.Copy(h, f); copyErr != nil {
			_ = f.Close()
			return "", fmt.Errorf("cache key hash %s: %w", p, copyErr)
		}
		f.Close()
	}

	for _, ef := range extraFiles {
		fmt.Fprintf(h, "extra:%s\n", ef)
	}

	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// CacheEntry records the result of a previous build.
type CacheEntry struct {
	Key         string `json:"key"`
	ImageDigest string `json:"image_digest"`
	SourceDir   string `json:"source_dir"`
	Lang        string `json:"lang"`
}

// Has checks whether a cache entry exists for the given key.
func (bc *BuildCache) Has(key string) bool {
	path := bc.entryPath(key)
	_, err := os.Stat(path)
	return err == nil
}

// Store writes a cache entry.
func (bc *BuildCache) Store(entry CacheEntry) error {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("build cache store: %w", err)
	}
	if err := os.WriteFile(bc.entryPath(entry.Key), data, 0o644); err != nil {
		return fmt.Errorf("build cache store: write %s: %w", bc.entryPath(entry.Key), err)
	}
	return nil
}

// Get reads a cache entry. Returns nil if not found.
func (bc *BuildCache) Get(key string) (*CacheEntry, error) {
	data, err := os.ReadFile(bc.entryPath(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("build cache get: %w", err)
	}
	var entry CacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("build cache parse: %w", err)
	}
	return &entry, nil
}

func (bc *BuildCache) entryPath(key string) string {
	return filepath.Join(bc.storeDir, key+".json")
}
