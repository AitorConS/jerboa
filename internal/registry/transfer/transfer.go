// Package transfer pushes images to and pulls them from a registry as
// recipes and blobs, assembling pulled disks locally.
package transfer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/pack"
	"github.com/AitorConS/jerboa/internal/registry/recipe"
	"github.com/klauspost/compress/zstd"
)

// MkfsFor returns the mkfs binary of a toolset version.
type MkfsFor func(ctx context.Context, version string) (string, error)

// PushOptions describe one push.
type PushOptions struct {
	Client *client.Client
	// Token is a push token for Ref's repository.
	Token  string
	Ref    naming.ImageReference
	Store  *cas.Store
	Recipe string
	Mkfs   MkfsFor
	// WorkDir holds the dry-run disk; empty means the system temp dir.
	WorkDir string
}

// PushResult reports what a push uploaded.
type PushResult struct {
	Recipe        string `json:"recipe"`
	Blobs         int    `json:"blobs"`
	BlobsUploaded int    `json:"blobs_uploaded"`
	PacksUploaded int    `json:"packs_uploaded"`
	BytesUploaded int64  `json:"bytes_uploaded"`
}

// Push publishes a recipe and tags it. It first assembles the image from the
// recipe and the local blob store, so it never publishes a recipe that does
// not rebuild its disk, then uploads only the blobs the registry lacks.
func Push(ctx context.Context, o PushOptions) (PushResult, error) {
	res := PushResult{Recipe: o.Recipe}
	data, err := o.Store.ReadBytes(o.Recipe)
	if err != nil {
		return res, fmt.Errorf("push: recipe: %w", err)
	}
	r, err := recipe.Parse(data)
	if err != nil {
		return res, fmt.Errorf("push: %w", err)
	}
	res.Blobs = len(r.Blobs)
	if err := dryRun(ctx, r, o.Store, o.Mkfs, o.WorkDir); err != nil {
		return res, err
	}

	digests := make([]string, len(r.Blobs))
	sizes := map[string]int64{}
	for i, b := range r.Blobs {
		digests[i] = b.Digest
		sizes[b.Digest] = b.Size
	}
	missing, err := o.Client.Missing(ctx, o.Token, o.Ref.Namespace, o.Ref.Repository, digests)
	if err != nil {
		return res, fmt.Errorf("push: %w", err)
	}
	w, err := pack.NewWriter(zstd.SpeedBestCompression)
	if err != nil {
		return res, fmt.Errorf("push: %w", err)
	}
	upload := func(packs []pack.Pack) error {
		for _, p := range packs {
			if err := o.Client.PutPack(ctx, o.Token, o.Ref.Namespace, o.Ref.Repository, p); err != nil {
				return fmt.Errorf("push: pack: %w", err)
			}
			res.PacksUploaded++
			res.BlobsUploaded += len(p.Entries)
			res.BytesUploaded += int64(len(p.Data))
		}
		return nil
	}
	for _, d := range missing {
		// Upload only this recipe's blobs, whatever the registry asks for.
		if _, ok := sizes[d]; !ok {
			return res, fmt.Errorf("push: the registry asked for blob %s, which this image does not use", d)
		}
		path, err := o.Store.Path(d)
		if err != nil {
			return res, fmt.Errorf("push: %w", err)
		}
		if err := w.Add(pack.Blob{Digest: d, Size: sizes[d], Open: func() (io.ReadCloser, error) { return os.Open(path) }}); err != nil {
			return res, fmt.Errorf("push: %w", err)
		}
		if err := upload(w.Take()); err != nil {
			return res, err
		}
	}
	if err := upload(w.Close()); err != nil {
		return res, err
	}
	if err := o.Client.PutRecipe(ctx, o.Token, o.Ref.Namespace, o.Ref.Repository, o.Recipe, data); err != nil {
		return res, fmt.Errorf("push: recipe: %w", err)
	}
	if err := o.Client.PutTag(ctx, o.Token, o.Ref.Namespace, o.Ref.Repository, o.Ref.Tag, o.Recipe); err != nil {
		return res, fmt.Errorf("push: tag: %w", err)
	}
	return res, nil
}

func dryRun(ctx context.Context, r *recipe.Recipe, store *cas.Store, mkfsFor MkfsFor, dir string) error {
	mkfs, err := mkfsFor(ctx, r.Toolset.Version)
	if err != nil {
		return fmt.Errorf("push: mkfs %s: %w", r.Toolset.Version, err)
	}
	tmp, err := os.MkdirTemp(dir, "jerboa-push-*")
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := recipe.Assemble(ctx, r, store, mkfs, filepath.Join(tmp, "disk.img")); err != nil {
		return fmt.Errorf("push: the recipe does not rebuild this image: %w", err)
	}
	return nil
}

// PullOptions describe one pull.
type PullOptions struct {
	Client *client.Client
	Ref    naming.ImageReference
	Store  *cas.Store
	Mkfs   MkfsFor
	// Out is where the assembled disk is written.
	Out string
	// Parallel bounds concurrent range requests; zero means 4.
	Parallel int
	// NoCount skips reporting the pull to the hub's counter.
	NoCount bool
}

// PullResult reports what a pull fetched.
type PullResult struct {
	Recipe        string         `json:"recipe"`
	Parsed        *recipe.Recipe `json:"-"`
	Blobs         int            `json:"blobs"`
	BlobsFetched  int            `json:"blobs_fetched"`
	Requests      int            `json:"requests"`
	BytesFetched  int64          `json:"bytes_fetched"`
	AlreadyCached bool           `json:"-"`
}

// maxRangeGap merges frames of a pack closer than this into one request.
const maxRangeGap = 64 << 10

// Pull fetches a recipe and the blobs the local store lacks, verifying each
// one, then assembles the disk at o.Out and verifies it against the recipe.
func Pull(ctx context.Context, o PullOptions) (PullResult, error) {
	var res PullResult
	d, err := o.Client.Discover(ctx)
	if err != nil {
		return res, fmt.Errorf("pull: %w", err)
	}
	digest := o.Ref.Digest
	if digest == "" {
		if digest, err = o.Client.TagRecipe(ctx, d.CDN, o.Ref.Namespace, o.Ref.Repository, o.Ref.Tag); err != nil {
			return res, fmt.Errorf("pull: %w", err)
		}
	} else if err := o.Client.HasRecipe(ctx, d.CDN, o.Ref.Namespace, o.Ref.Repository, digest); err != nil {
		return res, fmt.Errorf("pull: %w", err)
	}
	data, err := o.Client.Recipe(ctx, d.CDN, digest)
	if err != nil {
		return res, fmt.Errorf("pull: %w", err)
	}
	r, err := recipe.Parse(data)
	if err != nil {
		return res, fmt.Errorf("pull: %w", err)
	}
	res.Recipe, res.Parsed, res.Blobs = digest, r, len(r.Blobs)
	if _, err := o.Store.PutBytes(data); err != nil {
		return res, fmt.Errorf("pull: %w", err)
	}

	sizes := map[string]int64{}
	var missing []string
	for _, b := range r.Blobs {
		sizes[b.Digest] = b.Size
		if !o.Store.Has(b.Digest) {
			missing = append(missing, b.Digest)
		}
	}
	if len(missing) > 0 {
		if err := fetch(ctx, o, d.CDN, digest, missing, sizes, &res); err != nil {
			return res, err
		}
	}
	mkfs, err := o.Mkfs(ctx, r.Toolset.Version)
	if err != nil {
		return res, fmt.Errorf("pull: mkfs %s: %w", r.Toolset.Version, err)
	}
	if err := recipe.Assemble(ctx, r, o.Store, mkfs, o.Out); err != nil {
		return res, fmt.Errorf("pull: %w", err)
	}
	// The hub's pull counter: best effort, bounded, never fails the pull.
	if !o.NoCount {
		countCtx, cancel := context.WithTimeout(ctx, countPullTimeout)
		_ = o.Client.CountPull(countCtx, o.Ref.Namespace, o.Ref.Repository)
		cancel()
	}
	return res, nil
}

// DoNotTrack reports whether the environment asks not to report pulls, by the
// DO_NOT_TRACK convention: any value but empty, "0" or "false".
func DoNotTrack() bool {
	v := strings.TrimSpace(os.Getenv("DO_NOT_TRACK"))
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

// countPullTimeout bounds how long a pull waits to report itself.
const countPullTimeout = 2 * time.Second

func fetch(ctx context.Context, o PullOptions, cdn, digest string, missing []string, sizes map[string]int64, res *PullResult) error {
	locs, err := o.Client.Locations(ctx, cdn, digest)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	want := map[string]pack.Location{}
	for _, d := range missing {
		l, ok := locs[d]
		if !ok {
			return fmt.Errorf("pull: the registry does not say where blob %s is", d)
		}
		if err := l.Validate(); err != nil {
			return fmt.Errorf("pull: blob %s: %w", d, err)
		}
		want[d] = l
	}
	ranges := pack.PlanRanges(want, sizes, maxRangeGap)
	parallel := o.Parallel
	if parallel <= 0 {
		parallel = 4
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
		sem      = make(chan struct{}, parallel)
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	for _, rg := range ranges {
		wg.Add(1)
		sem <- struct{}{}
		go func(rg pack.Range) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					fail(fmt.Errorf("pull: pack %s: %v", rg.Pack, r))
				}
			}()
			data, err := o.Client.ReadRange(ctx, cdn, rg.Pack, rg.Offset, rg.Length)
			if err != nil {
				fail(fmt.Errorf("pull: %w", err))
				return
			}
			for _, e := range rg.Entries {
				start := e.Offset - rg.Offset
				if start < 0 || e.Length <= 0 || start > int64(len(data))-e.Length {
					fail(fmt.Errorf("pull: blob %s is outside the range read", e.Digest))
					return
				}
				blob, err := pack.ReadBlob(data[start:start+e.Length], e)
				if err != nil {
					fail(fmt.Errorf("pull: %w", err))
					return
				}
				got, err := o.Store.PutBytes(blob)
				if err != nil {
					fail(fmt.Errorf("pull: store blob %s: %w", e.Digest, err))
					return
				}
				if got != e.Digest {
					fail(fmt.Errorf("pull: stored blob %s as %s", e.Digest, got))
					return
				}
			}
			mu.Lock()
			res.Requests++
			res.BlobsFetched += len(rg.Entries)
			res.BytesFetched += rg.Length
			mu.Unlock()
		}(rg)
	}
	wg.Wait()
	return firstErr
}
