package builder

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// cacheIdleLimit is how long a project's tool caches are kept after its last
// build: each project path ever built (temporary directories and CI
// checkouts included) gets caches of its own, so they are pruned by age.
const cacheIdleLimit = 30 * 24 * time.Hour

var pruneCachesOnce sync.Once

// markCacheUsed records that base was used now, and once per process prunes
// the caches beside it that were not used for cacheIdleLimit. Pruning is
// best effort: a cache it cannot remove is left for the next run.
func markCacheUsed(base string) {
	now := time.Now()
	_ = os.Chtimes(base, now, now)
	pruneCachesOnce.Do(func() { pruneIdleCaches(filepath.Dir(base), base, now) })
}

// pruneIdleCaches removes the cache directories under parent, other than
// keep, whose mark is older than cacheIdleLimit.
func pruneIdleCaches(parent, keep string, now time.Time) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || filepath.Join(parent, e.Name()) == keep {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < cacheIdleLimit {
			continue
		}
		if err := removeCache(parent, e.Name()); err != nil {
			slog.Warn("build sandbox: could not remove an unused tool cache", "dir", filepath.Join(parent, e.Name()), "err", err)
		}
	}
}

// removeCache removes parent/name. Tools leave read-only directories (the Go
// module cache), which are made writable first. Everything goes through
// os.Root, so a link a build step left in the cache leads nowhere outside it.
func removeCache(parent, name string) error {
	root, err := os.OpenRoot(parent)
	if err != nil {
		return fmt.Errorf("open cache directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	sub, err := root.OpenRoot(name)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	_ = fs.WalkDir(sub.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = sub.Chmod(rel, 0o700)
		}
		return nil
	})
	_ = sub.Close()
	if err := root.RemoveAll(name); err != nil {
		return fmt.Errorf("remove cache: %w", err)
	}
	return nil
}
