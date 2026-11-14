//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/registry/cas"
	"github.com/AitorConS/jerboa/internal/registry/recipe"
	"github.com/AitorConS/jerboa/internal/vm"
)

// SystemPaths are the directories the daemon keeps its data in, for the
// commands that measure, clean and check them. An empty path leaves that part
// out of the report.
type SystemPaths struct {
	// DataDir is the directory everything else normally lives under; its
	// filesystem is the one free space is reported for.
	DataDir string
	// VMs holds one directory per VM with its persisted state.
	VMs string
	// VMDisks is the directory dedicated to private boot disks.
	VMDisks string
	// Tools holds the downloaded kernel toolsets.
	Tools string
}

// SetSystemPaths tells the server where the daemon's data lives, enabling
// System.DF, System.Prune and System.Check to cover those directories.
func (s *Server) SetSystemPaths(p SystemPaths) {
	s.paths = p
}

// leftoverGrace keeps prune away from temporary files and staging directories
// that an operation still running (a pull, an import) may be writing.
const leftoverGrace = time.Hour

// allocated returns the space a file really occupies. Volumes and VM disks
// are sparse: their length says how big they may grow, not what they hold.
func allocated(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512 //nolint:unconvert // Blocks is int64 on some platforms and not others
	}
	return info.Size()
}

// treeUsage returns the number of regular files under root and the space they
// occupy. A missing root is empty, not an error.
func treeUsage(root string) (files int, bytes int64) {
	if root == "" {
		return 0, 0
	}
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is skipped, the rest still counts
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			files++
			bytes += allocated(info)
		}
		return nil
	})
	return files, bytes
}

func fileUsage(path string) int64 {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	if info.IsDir() {
		_, b := treeUsage(path)
		return b
	}
	return allocated(info)
}

func (s *Server) vms() []*vm.VM {
	if s.mgr == nil {
		return nil
	}
	return s.mgr.List()
}

// volumesInUse returns the cleaned disk paths of every volume a VM mounts.
func volumesInUse(vms []*vm.VM) map[string]bool {
	out := make(map[string]bool)
	for _, v := range vms {
		for _, m := range v.Cfg.Volumes {
			out[filepath.Clean(m.DiskPath)] = true
		}
	}
	return out
}

// recipeBlobsInUse returns the blobs the recipes of the stored images need.
// ok is false when a recipe cannot be read, in which case nothing may be
// assumed unused.
func (s *Server) recipeBlobsInUse() (keep map[string]bool, ok bool) {
	keep = map[string]bool{}
	if s.recipes == nil || s.imgStore == nil {
		return keep, false
	}
	images, err := s.imgStore.List()
	if err != nil {
		return keep, false
	}
	for _, m := range images {
		if m.Recipe == "" || keep[m.Recipe] {
			continue
		}
		keep[m.Recipe] = true
		r, err := recipe.Load(s.recipes, m.Recipe)
		if err != nil {
			return keep, false
		}
		for _, b := range r.Blobs {
			keep[b.Digest] = true
		}
	}
	return keep, true
}

func (s *Server) handleSystemDF() (any, *api.RPCError) {
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	return s.systemDF(), nil
}

func (s *Server) systemDF() api.SystemDF {
	vms := s.vms()
	out := api.SystemDF{DataDir: s.paths.DataDir}
	out.Total, out.Free = filesystemSpace(s.paths.DataDir)

	// Images: one entry per stored disk, however many names point at it.
	if s.imgStore != nil {
		u := api.SystemUsage{Kind: "images"}
		inUse := vm.ImageDigestsInUse(vms)
		digests, _ := s.imgStore.Digests()
		for _, d := range digests {
			size := fileUsage(filepath.Join(s.imgStore.Root(), strings.TrimPrefix(d, "sha256:")))
			u.Count++
			u.Bytes += size
			if inUse[d] {
				u.Active++
			} else {
				u.Reclaimable += size
			}
		}
		out.Usage = append(out.Usage, u)
	}

	// VMs: their records and the private boot disks of those running.
	{
		u := api.SystemUsage{Kind: "vms", Count: len(vms)}
		for _, v := range vms {
			if st := v.GetState(); st != vm.StateStopped && st != vm.StateCreated {
				u.Active++
			}
		}
		_, records := treeUsage(s.paths.VMs)
		_, disks := treeUsage(s.paths.VMDisks)
		u.Bytes = records + disks
		for _, p := range vm.OrphanBootDisks(s.paths.VMDisks, vms) {
			u.Reclaimable += fileUsage(p)
		}
		out.Usage = append(out.Usage, u)
	}

	if s.volStore != nil {
		u := api.SystemUsage{Kind: "volumes"}
		mounted := volumesInUse(vms)
		vols, _ := s.volStore.List()
		for _, v := range vols {
			size := fileUsage(filepath.Dir(v.DiskPath))
			u.Count++
			u.Bytes += size
			if mounted[filepath.Clean(v.DiskPath)] {
				u.Active++
			} else {
				u.Reclaimable += size
			}
		}
		for _, dir := range s.volStore.Staging() {
			size := fileUsage(dir)
			u.Bytes += size
			u.Reclaimable += size
		}
		out.Usage = append(out.Usage, u)
	}

	if sm, ok := s.mgr.(snapshotManager); ok {
		u := api.SystemUsage{Kind: "snapshots"}
		if entries, err := sm.SnapshotList(); err == nil {
			u.Count = len(entries)
		}
		if d, ok := s.mgr.(interface{ SnapshotDir() string }); ok {
			_, u.Bytes = treeUsage(d.SnapshotDir())
		}
		out.Usage = append(out.Usage, u)
	}

	if s.recipes != nil {
		u := api.SystemUsage{Kind: "build-cache"}
		keep, known := s.recipeBlobsInUse()
		entries, _ := os.ReadDir(s.recipes.Root())
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			size := allocated(info)
			u.Count++
			u.Bytes += size
			if _, isBlob := cas.ParseDigest(cas.Prefix + e.Name()); !known || !isBlob {
				continue
			}
			if keep[cas.Prefix+e.Name()] {
				u.Active++
			} else {
				u.Reclaimable += size
			}
		}
		out.Usage = append(out.Usage, u)
	}

	if s.paths.Tools != "" {
		u := api.SystemUsage{Kind: "tools"}
		u.Count, u.Bytes = treeUsage(s.paths.Tools)
		u.Active = u.Count
		out.Usage = append(out.Usage, u)
	}
	return out
}

// filesystemSpace is statfsSpace, replaceable so tests can report a full disk.
var filesystemSpace = statfsSpace

// statfsSpace returns the size and the space available to the daemon on the
// filesystem holding dir, or zeros when it cannot tell.
func statfsSpace(dir string) (total, free int64) {
	if dir == "" {
		return 0, 0
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)                               //nolint:unconvert // Bsize differs in type across platforms
	return int64(st.Blocks) * bs, int64(st.Bavail) * bs //nolint:gosec,unconvert // block counts fit in int64
}

func (s *Server) handleSystemPrune(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	var p api.SystemPruneParams
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
		}
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	return s.systemPrune(ctx, p), nil
}

// systemPrune removes what nothing refers to any more and whatever p adds.
// The caller holds resourceMu, so no VM is created or started meanwhile.
func (s *Server) systemPrune(ctx context.Context, p api.SystemPruneParams) api.SystemPruneResult {
	res := api.SystemPruneResult{DryRun: p.DryRun, Removed: []api.SystemPruneItem{}}
	add := func(kind, name string, bytes int64) {
		res.Removed = append(res.Removed, api.SystemPruneItem{Kind: kind, Name: name, Bytes: bytes})
		res.Reclaimed += bytes
	}
	fail := func(format string, args ...any) {
		res.Errors = append(res.Errors, fmt.Sprintf(format, args...))
	}

	// Stopped VMs go first: what they were holding becomes unused below.
	remaining := s.vms()
	if p.VMs {
		var kept []*vm.VM
		for _, v := range remaining {
			if v.GetState() != vm.StateStopped {
				kept = append(kept, v)
				continue
			}
			name := v.ID
			if v.Cfg.Name != "" {
				name = v.Cfg.Name + " (" + v.ID + ")"
			}
			size := fileUsage(filepath.Join(s.paths.VMs, v.ID))
			if s.paths.VMs == "" {
				size = 0
			}
			if !p.DryRun {
				if err := s.removeVMLocked(ctx, v.ID); err != nil {
					fail("vm %s: %v", name, err)
					kept = append(kept, v)
					continue
				}
			}
			add("vm", name, size)
		}
		remaining = kept
	}

	s.pruneImageStore(p, remaining, add, fail)
	s.pruneVolumes(p, remaining, add, fail)

	for _, disk := range vm.OrphanBootDisks(s.paths.VMDisks, remaining) {
		size := fileUsage(disk)
		if !p.DryRun {
			if err := os.Remove(disk); err != nil && !errors.Is(err, fs.ErrNotExist) {
				fail("boot disk %s: %v", filepath.Base(disk), err)
				continue
			}
		}
		add("boot-disk", filepath.Base(disk), size)
	}

	s.pruneBlobs(p, add, fail)
	s.pruneVMRecords(p, remaining, add, fail)

	sort.SliceStable(res.Removed, func(i, j int) bool { return res.Removed[i].Kind < res.Removed[j].Kind })
	return res
}

type (
	pruneAdd  func(kind, name string, bytes int64)
	pruneFail func(format string, args ...any)
)

func (s *Server) pruneImageStore(p api.SystemPruneParams, vms []*vm.VM, add pruneAdd, fail pruneFail) {
	if s.imgStore == nil {
		return
	}
	root := s.imgStore.Root()
	inUse := vm.ImageDigestsInUse(vms)
	short := func(digest string) string {
		hex := strings.TrimPrefix(digest, "sha256:")
		if len(hex) > 12 {
			hex = hex[:12]
		}
		return hex
	}

	if p.All {
		// Every name whose disk no VM uses; the disk goes with its last name.
		refs, err := s.imgStore.Refs()
		if err != nil {
			fail("images: %v", err)
		}
		names := make([]string, 0, len(refs))
		for ref := range refs {
			names = append(names, ref)
		}
		sort.Strings(names)
		counted := map[string]bool{}
		for _, ref := range names {
			digest := refs[ref]
			dir := filepath.Join(root, strings.TrimPrefix(digest, "sha256:"))
			if _, err := os.Stat(dir); inUse[digest] || os.IsNotExist(err) {
				continue // in use, or a dangling name reported below
			}
			size := int64(0)
			if !counted[digest] {
				counted[digest] = true
				size = fileUsage(filepath.Join(root, strings.TrimPrefix(digest, "sha256:")))
			}
			if !p.DryRun {
				if err := s.imgStore.Remove(ref); err != nil {
					fail("image %s: %v", ref, err)
					continue
				}
			}
			add("image", ref, size)
		}
	}

	// Names whose disk is gone name nothing; the prune below drops them.
	if refs, err := s.imgStore.Refs(); err == nil {
		names := make([]string, 0, len(refs))
		for ref := range refs {
			names = append(names, ref)
		}
		sort.Strings(names)
		for _, ref := range names {
			dir := filepath.Join(root, strings.TrimPrefix(refs[ref], "sha256:"))
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				add("image", ref+" (its disk is already gone)", 0)
			}
		}
	}

	// Disks no name points at: what a rebuilt tag leaves behind.
	unreferenced, err := s.imgStore.Unreferenced(func(d string) bool { return inUse[d] })
	if err != nil {
		fail("images: %v", err)
	}
	sizes := make(map[string]int64, len(unreferenced))
	for _, d := range unreferenced {
		sizes[d] = fileUsage(filepath.Join(root, strings.TrimPrefix(d, "sha256:")))
	}
	if p.DryRun {
		for _, d := range unreferenced {
			add("image", "<untagged> "+short(d), sizes[d])
		}
	} else {
		removed, err := s.imgStore.PruneUnreferenced(func(d string) bool { return inUse[d] })
		if err != nil {
			fail("images: %v", err)
		}
		for _, d := range removed {
			add("image", "<untagged> "+short(d), sizes[d])
		}
	}

	// Temporary files of writes, copies and pulls that never finished.
	entries, _ := os.ReadDir(root)
	cutoff := time.Now().Add(-leftoverGrace)
	for _, e := range entries {
		if e.IsDir() || e.Name() == "refs.json" {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if !durable.IsTemp(e.Name()) && !strings.HasPrefix(e.Name(), ".pull-") && e.Name() != "refs.json.corrupt" {
			continue
		}
		size := allocated(info)
		if !p.DryRun {
			if err := os.Remove(filepath.Join(root, e.Name())); err != nil {
				fail("leftover %s: %v", e.Name(), err)
				continue
			}
		}
		add("leftover", "images/"+e.Name(), size)
	}
}

func (s *Server) pruneVolumes(p api.SystemPruneParams, vms []*vm.VM, add pruneAdd, fail pruneFail) {
	if s.volStore == nil {
		return
	}
	if p.Volumes {
		mounted := volumesInUse(vms)
		vols, err := s.volStore.List()
		if err != nil {
			fail("volumes: %v", err)
		}
		for _, v := range vols {
			if mounted[filepath.Clean(v.DiskPath)] {
				continue
			}
			size := fileUsage(filepath.Dir(v.DiskPath))
			if !p.DryRun {
				// The same check `volume rm` makes, which also catches a VM
				// that reaches the disk through another path.
				if err := s.volumeUnused(v.DiskPath); err != nil {
					continue
				}
				if err := s.volStore.Remove(v.ID); err != nil {
					fail("volume %s: %v", v.ID, err)
					continue
				}
			}
			add("volume", v.ID, size)
		}
	}
	cutoff := time.Now().Add(-leftoverGrace)
	for _, dir := range s.volStore.Staging() {
		if info, err := os.Stat(dir); err != nil || info.ModTime().After(cutoff) {
			continue
		}
		add("leftover", "volumes/"+filepath.Base(dir), fileUsage(dir))
	}
	if !p.DryRun {
		s.volStore.SweepStaging(leftoverGrace)
	}
}

func (s *Server) pruneBlobs(p api.SystemPruneParams, add pruneAdd, fail pruneFail) {
	if s.recipes == nil {
		return
	}
	keep, known := s.recipeBlobsInUse()
	if !known {
		// A recipe that cannot be read may need any blob: keep them all.
		return
	}
	root := s.recipes.Root()
	size := func(digest string) int64 {
		return fileUsage(filepath.Join(root, strings.TrimPrefix(digest, cas.Prefix)))
	}
	if p.DryRun {
		entries, _ := os.ReadDir(root)
		cutoff := time.Now().Add(-recipeBlobGrace)
		for _, e := range entries {
			digest := cas.Prefix + e.Name()
			info, err := e.Info()
			if _, ok := cas.ParseDigest(digest); !ok || keep[digest] || err != nil || info.ModTime().After(cutoff) {
				continue
			}
			add("build-cache", e.Name()[:12], allocated(info))
		}
		return
	}
	sizes := map[string]int64{}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		sizes[cas.Prefix+e.Name()] = size(cas.Prefix + e.Name())
	}
	removed, err := s.recipes.Prune(func(d string) bool { return keep[d] }, recipeBlobGrace)
	if err != nil {
		fail("build cache: %v", err)
	}
	for _, d := range removed {
		add("build-cache", strings.TrimPrefix(d, cas.Prefix)[:12], sizes[d])
	}
}

// pruneVMRecords removes directories in the VM registry that belong to no VM:
// what is left of a record that could not be read back.
func (s *Server) pruneVMRecords(p api.SystemPruneParams, vms []*vm.VM, add pruneAdd, fail pruneFail) {
	for _, dir := range s.strayVMRecords(vms, leftoverGrace) {
		size := fileUsage(dir)
		if !p.DryRun {
			if err := os.RemoveAll(dir); err != nil {
				fail("vm record %s: %v", filepath.Base(dir), err)
				continue
			}
		}
		add("leftover", "vms/"+filepath.Base(dir), size)
	}
}

// strayVMRecords returns the directories in the VM registry that no known VM
// owns and that have not changed for longer than grace.
func (s *Server) strayVMRecords(vms []*vm.VM, grace time.Duration) []string {
	if s.paths.VMs == "" {
		return nil
	}
	entries, err := os.ReadDir(s.paths.VMs)
	if err != nil {
		return nil
	}
	known := make(map[string]bool, len(vms))
	for _, v := range vms {
		known[v.ID] = true
	}
	// A VM that this call removed (or would) still has its directory in a
	// dry run; it is not stray.
	for _, v := range s.vms() {
		known[v.ID] = true
	}
	cutoff := time.Now().Add(-grace)
	var out []string
	for _, e := range entries {
		if !e.IsDir() || known[e.Name()] {
			continue
		}
		if info, err := e.Info(); err != nil || info.ModTime().After(cutoff) {
			continue
		}
		out = append(out, filepath.Join(s.paths.VMs, e.Name()))
	}
	return out
}
