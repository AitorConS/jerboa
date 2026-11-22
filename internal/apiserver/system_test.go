//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	hypervisorpkg "github.com/AitorConS/jerboa/internal/hypervisor"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/snapshot"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sysEnv is a daemon's worth of stores in a temporary directory.
type sysEnv struct {
	t    *testing.T
	s    *Server
	mgr  *vm.MockManager
	root string
}

func newSysEnv(t *testing.T) *sysEnv {
	t.Helper()
	root := t.TempDir()
	img, err := image.NewStore(filepath.Join(root, "images"))
	require.NoError(t, err)
	vol, err := volume.NewStore(filepath.Join(root, "volumes"))
	require.NoError(t, err)
	nets, err := network.NewStore(filepath.Join(root, "networks"))
	require.NoError(t, err)
	for _, d := range []string{"vms", "vm-disks", "tools"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, d), 0o700))
	}
	mgr := vm.NewMockManager()
	s := &Server{mgr: mgr, imgStore: img, volStore: vol, netStore: nets}
	s.SetSystemPaths(SystemPaths{
		DataDir: root, VMs: filepath.Join(root, "vms"),
		VMDisks: filepath.Join(root, "vm-disks"), Tools: filepath.Join(root, "tools"),
	})
	return &sysEnv{t: t, s: s, mgr: mgr, root: root}
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// image stores content as name:tag and returns the path of its disk.
func (e *sysEnv) image(name, tag, content string) string {
	e.t.Helper()
	src := filepath.Join(e.t.TempDir(), "disk.img")
	require.NoError(e.t, os.WriteFile(src, []byte(content), 0o644))
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion, Name: name, Tag: tag, Created: time.Now().UTC(),
		Config: image.Config{Memory: "256M", CPUs: 1}, DiskSize: int64(len(content)),
	}
	require.NoError(e.t, e.s.imgStore.Put(name, tag, m, src))
	return filepath.Join(e.root, "images", sha(content), "disk.img")
}

// untagged leaves a stored disk that no name points at, as a rebuild does,
// and returns its directory.
func (e *sysEnv) untagged(content string) string {
	e.t.Helper()
	e.image("tmp-untagged", "x", content)
	path := filepath.Join(e.root, "images", "refs.json")
	data, err := os.ReadFile(path)
	require.NoError(e.t, err)
	refs := map[string]string{}
	require.NoError(e.t, json.Unmarshal(data, &refs))
	delete(refs, "tmp-untagged:x")
	data, err = json.Marshal(refs)
	require.NoError(e.t, err)
	require.NoError(e.t, os.WriteFile(path, data, 0o644))
	return filepath.Join(e.root, "images", sha(content))
}

type vmState int

const (
	created vmState = iota
	running
	stopped
)

func (e *sysEnv) vm(name string, state vmState, cfg vm.Config) *vm.VM {
	e.t.Helper()
	cfg.Name = name
	ctx := context.Background()
	v, err := e.mgr.Create(ctx, cfg)
	require.NoError(e.t, err)
	if state >= running {
		require.NoError(e.t, e.mgr.Start(ctx, v.ID))
	}
	if state == stopped {
		require.NoError(e.t, e.mgr.Stop(ctx, v.ID))
	}
	dir := filepath.Join(e.root, "vms", v.ID)
	require.NoError(e.t, os.Mkdir(dir, 0o700))
	require.NoError(e.t, os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"id":"`+v.ID+`"}`), 0o600))
	return v
}

func (e *sysEnv) volume(name string) *volume.Volume {
	e.t.Helper()
	v, err := e.s.volStore.Create(name, 256<<20)
	require.NoError(e.t, err)
	return v
}

// aged writes a file and backdates it past the leftover grace period.
func (e *sysEnv) aged(rel, content string) string {
	e.t.Helper()
	path := filepath.Join(e.root, rel)
	require.NoError(e.t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(e.t, os.WriteFile(path, []byte(content), 0o600))
	old := time.Now().Add(-2 * leftoverGrace)
	require.NoError(e.t, os.Chtimes(path, old, old))
	if dir := filepath.Dir(path); dir != filepath.Join(e.root, "images") && dir != filepath.Join(e.root, "vm-disks") {
		require.NoError(e.t, os.Chtimes(dir, old, old))
	}
	return path
}

func (e *sysEnv) usage(kind string) api.SystemUsage {
	e.t.Helper()
	for _, u := range e.s.systemDF().Usage {
		if u.Kind == kind {
			return u
		}
	}
	e.t.Fatalf("no usage reported for %s", kind)
	return api.SystemUsage{}
}

func (e *sysEnv) prune(p api.SystemPruneParams) api.SystemPruneResult {
	e.t.Helper()
	return e.s.systemPrune(context.Background(), p)
}

func names(res api.SystemPruneResult, kind string) []string {
	var out []string
	for _, it := range res.Removed {
		if it.Kind == kind {
			out = append(out, it.Name)
		}
	}
	sort.Strings(out)
	return out
}

// tree lists every path under root, to prove that nothing changed.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out = append(out, strings.TrimPrefix(p, root))
		return nil
	}))
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- System.DF ---------------------------------------------------------------

func TestSystemDF_Empty(t *testing.T) {
	e := newSysEnv(t)
	df := e.s.systemDF()
	var kinds []string
	for _, u := range df.Usage {
		kinds = append(kinds, u.Kind)
		assert.Zero(t, u.Count, u.Kind)
		assert.Zero(t, u.Reclaimable, u.Kind)
	}
	assert.Equal(t, []string{"images", "vms", "volumes", "tools"}, kinds)
	assert.Equal(t, e.root, df.DataDir)
	assert.Positive(t, df.Total)
	assert.LessOrEqual(t, df.Free, df.Total)
}

func TestSystemDF_ImagesCountDisksNotNames(t *testing.T) {
	e := newSysEnv(t)
	used := e.image("web", "latest", strings.Repeat("web ", 5000))
	_, err := e.s.imgStore.Tag("web:latest", "web", "v1")
	require.NoError(t, err)
	e.image("idle", "latest", strings.Repeat("idle ", 9000))
	e.untagged(strings.Repeat("old build ", 7000))
	e.vm("app", running, vm.Config{ImagePath: used})

	u := e.usage("images")
	assert.Equal(t, 3, u.Count, "two names on one disk are one image on disk")
	assert.Equal(t, 1, u.Active)
	assert.Positive(t, u.Reclaimable)
	assert.Less(t, u.Reclaimable, u.Bytes, "the image a VM uses is not reclaimable")
	assert.GreaterOrEqual(t, u.Bytes, int64(20000+45000+70000))
}

func TestSystemDF_ImageOfStoppedVMIsStillInUse(t *testing.T) {
	e := newSysEnv(t)
	disk := e.image("web", "latest", "web disk")
	e.vm("app", stopped, vm.Config{ImagePath: disk})
	u := e.usage("images")
	assert.Equal(t, 1, u.Active, "a stopped VM boots from its image again on start")
	assert.Zero(t, u.Reclaimable)
}

func TestSystemDF_VolumesReportWhatTheyHoldNotTheirSize(t *testing.T) {
	e := newSysEnv(t)
	mounted := e.volume("data")
	e.volume("spare")
	e.vm("db", running, vm.Config{Volumes: []vm.VolumeMount{{DiskPath: mounted.DiskPath, GuestPath: "/data"}}})

	u := e.usage("volumes")
	assert.Equal(t, 2, u.Count)
	assert.Equal(t, 1, u.Active)
	assert.Less(t, u.Bytes, int64(64<<20), "two empty 256 MiB sparse volumes do not take 512 MiB")
	assert.Positive(t, u.Reclaimable)
	assert.Less(t, u.Reclaimable, u.Bytes)
}

func TestSystemDF_VMsAndOrphanDisks(t *testing.T) {
	e := newSysEnv(t)
	live := e.vm("live", running, vm.Config{})
	e.vm("done", stopped, vm.Config{})
	e.vm("new", created, vm.Config{})
	require.NoError(t, os.WriteFile(filepath.Join(e.root, "vm-disks", "qemu-"+live.ID+"-boot.img"), make([]byte, 50000), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(e.root, "vm-disks", "fc-gone-rootfs.img"), make([]byte, 90000), 0o600))

	u := e.usage("vms")
	assert.Equal(t, 3, u.Count)
	assert.Equal(t, 1, u.Active)
	assert.GreaterOrEqual(t, u.Bytes, int64(140000))
	assert.GreaterOrEqual(t, u.Reclaimable, int64(90000), "the disk of a VM that is gone is reclaimable")
	assert.Less(t, u.Reclaimable, int64(140000), "the disk of a running VM is not")
}

func TestSystemDF_Tools(t *testing.T) {
	e := newSysEnv(t)
	require.NoError(t, os.MkdirAll(filepath.Join(e.root, "tools", "v0.2.4"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(e.root, "tools", "v0.2.4", "kernel.img"), make([]byte, 30000), 0o600))
	u := e.usage("tools")
	assert.Equal(t, 1, u.Count)
	assert.GreaterOrEqual(t, u.Bytes, int64(30000))
	assert.Zero(t, u.Reclaimable, "the kernel is never offered for removal")
}

func TestSystemDF_MissingDirectoriesAreEmpty(t *testing.T) {
	e := newSysEnv(t)
	e.s.SetSystemPaths(SystemPaths{DataDir: filepath.Join(e.root, "absent"), VMs: filepath.Join(e.root, "no-vms"), VMDisks: filepath.Join(e.root, "no-disks")})
	df := e.s.systemDF()
	assert.Zero(t, df.Total)
	assert.Zero(t, e.usage("vms").Bytes)
}

func TestSystemDF_WithoutStores(t *testing.T) {
	s := &Server{mgr: vm.NewMockManager()}
	df := s.systemDF()
	require.Len(t, df.Usage, 1)
	assert.Equal(t, "vms", df.Usage[0].Kind)
}

func TestTreeUsageAndFileUsage(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "one"), make([]byte, 10000), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "b", "two"), make([]byte, 20000), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(dir, "a", "one"), filepath.Join(dir, "link")))

	files, bytes := treeUsage(dir)
	assert.Equal(t, 2, files, "a symlink is not a second copy")
	assert.GreaterOrEqual(t, bytes, int64(30000))
	assert.Equal(t, bytes, fileUsage(dir))
	assert.GreaterOrEqual(t, fileUsage(filepath.Join(dir, "a", "one")), int64(10000))
	assert.Zero(t, fileUsage(filepath.Join(dir, "absent")))
	files, bytes = treeUsage("")
	assert.Zero(t, files)
	assert.Zero(t, bytes)
}

// --- System.Prune ------------------------------------------------------------

func TestSystemPrune_NothingToDo(t *testing.T) {
	e := newSysEnv(t)
	res := e.prune(api.SystemPruneParams{})
	assert.Empty(t, res.Removed)
	assert.NotNil(t, res.Removed, "an empty result is a list, not null, for JSON consumers")
	assert.Empty(t, res.Errors)
	assert.Zero(t, res.Reclaimed)
}

// The default prune must be safe to run blind: nothing that still has a name
// is touched.
func TestSystemPrune_DefaultKeepsEverythingNameable(t *testing.T) {
	e := newSysEnv(t)
	disk := e.image("web", "latest", "web disk")
	idle := e.image("idle", "latest", "idle disk")
	spare := e.volume("spare")
	done := e.vm("done", stopped, vm.Config{ImagePath: disk})
	recent := filepath.Join(e.root, "images", ".pull-recent.img")
	require.NoError(t, os.WriteFile(recent, []byte("a pull in progress"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(e.root, "volumes", ".import-recent"), 0o700))

	res := e.prune(api.SystemPruneParams{})
	assert.Empty(t, res.Removed)
	assert.Empty(t, res.Errors)
	for _, p := range []string{disk, idle, spare.DiskPath, recent, filepath.Join(e.root, "volumes", ".import-recent")} {
		assert.True(t, exists(p), "%s was removed", p)
	}
	_, err := e.mgr.Get(done.ID)
	require.NoError(t, err, "a stopped VM was removed without --vms")
}

func TestSystemPrune_DefaultRemovesWhatNothingRefersTo(t *testing.T) {
	e := newSysEnv(t)
	kept := e.image("web", "latest", "web disk")
	untagged := e.untagged("an old build")
	live := e.vm("live", running, vm.Config{ImagePath: kept})
	liveDisk := filepath.Join(e.root, "vm-disks", "qemu-"+live.ID+"-boot.img")
	require.NoError(t, os.WriteFile(liveDisk, []byte("in use"), 0o600))
	orphan := filepath.Join(e.root, "vm-disks", "fc-gone-rootfs.img")
	require.NoError(t, os.WriteFile(orphan, make([]byte, 40000), 0o600))
	pull := e.aged("images/.pull-123.img", "half a pull")
	temp := e.aged("images/.refs.json.tmp-9", "{half")
	corrupt := e.aged("images/refs.json.corrupt", "{bad")
	staging := e.aged("volumes/.import-77/disk.img", "half an import")
	stray := e.aged("vms/deadbeef/state.json", "x")
	unrelated := e.aged("images/notes.txt", "mine")

	res := e.prune(api.SystemPruneParams{})
	require.Empty(t, res.Errors)
	for _, p := range []string{untagged, orphan, pull, temp, corrupt, filepath.Dir(staging), filepath.Dir(stray)} {
		assert.False(t, exists(p), "%s should have been removed", p)
	}
	for _, p := range []string{kept, liveDisk, unrelated} {
		assert.True(t, exists(p), "%s must be kept", p)
	}
	assert.Equal(t, []string{"fc-gone-rootfs.img"}, names(res, "boot-disk"))
	assert.Equal(t, []string{"<untagged> " + sha("an old build")[:12]}, names(res, "image"))
	assert.Equal(t, []string{"images/.pull-123.img", "images/.refs.json.tmp-9", "images/refs.json.corrupt", "vms/deadbeef", "volumes/.import-77"}, names(res, "leftover"))

	var sum int64
	for _, it := range res.Removed {
		sum += it.Bytes
	}
	assert.Equal(t, sum, res.Reclaimed)
	assert.GreaterOrEqual(t, res.Reclaimed, int64(40000))

	again := e.prune(api.SystemPruneParams{})
	assert.Empty(t, again.Removed, "a second prune finds nothing left")
}

func TestSystemPrune_DropsNamesOfMissingDisks(t *testing.T) {
	e := newSysEnv(t)
	e.image("web", "latest", "web disk")
	gone := e.image("gone", "latest", "gone disk")
	require.NoError(t, os.RemoveAll(filepath.Dir(gone)))

	dry := e.prune(api.SystemPruneParams{DryRun: true})
	assert.Equal(t, []string{"gone:latest (its disk is already gone)"}, names(dry, "image"))
	refs, err := e.s.imgStore.Refs()
	require.NoError(t, err)
	assert.Contains(t, refs, "gone:latest", "a dry run changes nothing")

	res := e.prune(api.SystemPruneParams{})
	assert.Equal(t, names(dry, "image"), names(res, "image"))
	refs, err = e.s.imgStore.Refs()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"web:latest": "sha256:" + sha("web disk")}, refs)
}

// A dry run must be an exact preview: same items, and not one byte changed.
func TestSystemPrune_DryRunChangesNothingAndMatchesTheRealRun(t *testing.T) {
	for _, p := range []api.SystemPruneParams{
		{},
		{VMs: true},
		{All: true},
		{Volumes: true},
		{VMs: true, All: true, Volumes: true},
	} {
		t.Run(fmt.Sprintf("vms=%t,all=%t,volumes=%t", p.VMs, p.All, p.Volumes), func(t *testing.T) {
			e := newSysEnv(t)
			used := e.image("web", "latest", "web disk")
			e.image("idle", "latest", "idle disk")
			onlyStopped := e.image("batch", "latest", "batch disk")
			e.untagged("an old build")
			mounted := e.volume("data")
			e.volume("spare")
			held := e.volume("held")
			e.vm("live", running, vm.Config{ImagePath: used, Volumes: []vm.VolumeMount{{DiskPath: mounted.DiskPath}}})
			e.vm("done", stopped, vm.Config{ImagePath: onlyStopped, Volumes: []vm.VolumeMount{{DiskPath: held.DiskPath}}})
			require.NoError(t, os.WriteFile(filepath.Join(e.root, "vm-disks", "fc-gone-rootfs.img"), []byte("orphan"), 0o600))
			e.aged("volumes/.create-5/disk.img", "half")

			before := tree(t, e.root)
			vmsBefore := len(e.mgr.List())
			dryParams := p
			dryParams.DryRun = true
			dry := e.prune(dryParams)
			assert.True(t, dry.DryRun)
			assert.Equal(t, before, tree(t, e.root), "the dry run changed files")
			assert.Len(t, e.mgr.List(), vmsBefore, "the dry run removed a VM")

			real := e.prune(p)
			require.Empty(t, real.Errors)
			assert.False(t, real.DryRun)
			assert.Equal(t, dry.Removed, real.Removed, "the dry run did not predict the real run")
			assert.Equal(t, dry.Reclaimed, real.Reclaimed)
		})
	}
}

func TestSystemPrune_VMs(t *testing.T) {
	e := newSysEnv(t)
	live := e.vm("live", running, vm.Config{})
	fresh := e.vm("fresh", created, vm.Config{})
	done := e.vm("done", stopped, vm.Config{})
	anon := e.vm("", stopped, vm.Config{})

	res := e.prune(api.SystemPruneParams{VMs: true})
	require.Empty(t, res.Errors)
	assert.Equal(t, []string{"done (" + done.ID + ")", anon.ID}, names(res, "vm"))
	var left []string
	for _, v := range e.mgr.List() {
		left = append(left, v.ID)
	}
	assert.ElementsMatch(t, []string{live.ID, fresh.ID}, left, "only stopped VMs go; one never started is not stopped")
}

func TestSystemPrune_VMsReleaseTheirAddresses(t *testing.T) {
	e := newSysEnv(t)
	_, err := e.s.netStore.Create("appnet", "10.80.0.0/24", "")
	require.NoError(t, err)
	ip, err := e.s.netStore.AllocateIP("appnet")
	require.NoError(t, err)
	e.vm("done", stopped, vm.Config{NetworkName: "appnet", IPAddress: ip.String()})

	res := e.prune(api.SystemPruneParams{VMs: true})
	require.Empty(t, res.Errors)
	require.NoError(t, e.s.netStore.ReserveIP("appnet", ip.String()), "the address of a pruned VM must be free again")
}

func TestSystemPrune_AllRemovesOnlyUnusedImages(t *testing.T) {
	e := newSysEnv(t)
	used := e.image("web", "latest", "web disk")
	_, err := e.s.imgStore.Tag("web:latest", "web", "v1")
	require.NoError(t, err)
	stoppedOnly := e.image("batch", "latest", "batch disk")
	idle := e.image("idle", "latest", "idle disk")
	_, err = e.s.imgStore.Tag("idle:latest", "idle", "v2")
	require.NoError(t, err)
	e.vm("live", running, vm.Config{ImagePath: used})
	e.vm("done", stopped, vm.Config{ImagePath: stoppedOnly})

	res := e.prune(api.SystemPruneParams{All: true})
	require.Empty(t, res.Errors)
	assert.Equal(t, []string{"idle:latest", "idle:v2"}, names(res, "image"))
	assert.False(t, exists(idle))
	assert.True(t, exists(used), "an image a running VM uses was removed")
	assert.True(t, exists(stoppedOnly), "a stopped VM still needs its image to start again")
	refs, err := e.s.imgStore.Refs()
	require.NoError(t, err)
	assert.Len(t, refs, 3)

	// Once the stopped VM goes too, so does the image only it used.
	res = e.prune(api.SystemPruneParams{All: true, VMs: true})
	require.Empty(t, res.Errors)
	assert.Equal(t, []string{"batch:latest"}, names(res, "image"))
	assert.False(t, exists(stoppedOnly))
	assert.True(t, exists(used))
}

func TestSystemPrune_AllCountsASharedDiskOnce(t *testing.T) {
	e := newSysEnv(t)
	e.image("idle", "latest", strings.Repeat("idle ", 4000))
	_, err := e.s.imgStore.Tag("idle:latest", "idle", "v2")
	require.NoError(t, err)
	size := e.usage("images").Bytes

	res := e.prune(api.SystemPruneParams{All: true})
	assert.Len(t, names(res, "image"), 2)
	assert.Equal(t, size, res.Reclaimed, "two names on one disk free that disk once")
}

func TestSystemPrune_Volumes(t *testing.T) {
	e := newSysEnv(t)
	mounted := e.volume("data")
	held := e.volume("held")
	spare := e.volume("spare")
	e.vm("live", running, vm.Config{Volumes: []vm.VolumeMount{{DiskPath: mounted.DiskPath, GuestPath: "/data"}}})
	e.vm("done", stopped, vm.Config{Volumes: []vm.VolumeMount{{DiskPath: held.DiskPath, GuestPath: "/data"}}})

	res := e.prune(api.SystemPruneParams{})
	assert.Empty(t, names(res, "volume"), "volumes hold data: never removed without --volumes")

	res = e.prune(api.SystemPruneParams{Volumes: true})
	require.Empty(t, res.Errors)
	assert.Equal(t, []string{"spare"}, names(res, "volume"))
	assert.False(t, exists(spare.DiskPath))
	assert.True(t, exists(mounted.DiskPath))
	assert.True(t, exists(held.DiskPath), "the volume of a stopped VM is still that VM's data")

	res = e.prune(api.SystemPruneParams{Volumes: true, VMs: true})
	assert.Equal(t, []string{"held"}, names(res, "volume"))
	assert.True(t, exists(mounted.DiskPath))
}

func TestSystemPrune_FailuresAreReportedAndTheRestGoesOn(t *testing.T) {
	e := newSysEnv(t)
	stuck := e.vm("stuck", stopped, vm.Config{})
	e.vm("fine", stopped, vm.Config{})
	orphan := filepath.Join(e.root, "vm-disks", "fc-gone-rootfs.img")
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o600))
	mgr := e.mgr
	// The hook steps aside to let the mock remove the other VMs and must be
	// back in place afterwards, whichever VM the prune reaches first.
	var hook func(context.Context, string) error
	hook = func(_ context.Context, id string) error {
		if id == stuck.ID {
			return errors.New("device busy")
		}
		mgr.RemoveFn = nil
		defer func() { mgr.RemoveFn = hook }()
		return mgr.Remove(context.Background(), id)
	}
	mgr.RemoveFn = hook

	res := e.prune(api.SystemPruneParams{VMs: true})
	require.Len(t, res.Errors, 1)
	assert.Contains(t, res.Errors[0], "stuck")
	assert.Contains(t, res.Errors[0], "device busy")
	assert.NotContains(t, strings.Join(names(res, "vm"), " "), "stuck", "a VM that could not be removed is not reported as removed")
	assert.Len(t, names(res, "vm"), 1)
	assert.False(t, exists(orphan), "one failure must not stop the rest of the prune")
	_, err := e.mgr.Get(stuck.ID)
	require.NoError(t, err)
}

func TestSystemPrune_SortedByKind(t *testing.T) {
	e := newSysEnv(t)
	e.image("idle", "latest", "idle disk")
	e.volume("spare")
	e.vm("done", stopped, vm.Config{})
	require.NoError(t, os.WriteFile(filepath.Join(e.root, "vm-disks", "fc-gone-rootfs.img"), []byte("x"), 0o600))
	res := e.prune(api.SystemPruneParams{VMs: true, All: true, Volumes: true, DryRun: true})
	var kinds []string
	for _, it := range res.Removed {
		kinds = append(kinds, it.Kind)
	}
	assert.True(t, sort.StringsAreSorted(kinds), "%v", kinds)
	assert.ElementsMatch(t, []string{"boot-disk", "image", "vm", "volume"}, kinds)
}

func TestHandleSystemPrune_Params(t *testing.T) {
	e := newSysEnv(t)
	e.volume("spare")
	ctx := context.Background()

	for _, raw := range []string{"", "null", "{}"} {
		got, rerr := e.s.handleSystemPrune(ctx, []byte(raw))
		require.Nil(t, rerr, "params %q", raw)
		assert.Empty(t, got.(api.SystemPruneResult).Removed)
	}
	_, rerr := e.s.handleSystemPrune(ctx, []byte(`{"volumes":"yes"}`))
	require.NotNil(t, rerr)
	assert.Equal(t, -32602, rerr.Code)

	got, rerr := e.s.handleSystemPrune(ctx, []byte(`{"volumes":true,"dry_run":true}`))
	require.Nil(t, rerr)
	assert.Equal(t, []string{"spare"}, names(got.(api.SystemPruneResult), "volume"))
}

func TestSystemDispatch(t *testing.T) {
	e := newSysEnv(t)
	ctx := context.Background()
	for method, want := range map[string]any{
		"System.DF":    api.SystemDF{},
		"System.Prune": api.SystemPruneResult{},
		"System.Check": api.SystemCheckResult{},
	} {
		got, rerr := e.s.dispatch(ctx, &api.Request{Method: method}, nil, nil)
		require.Nil(t, rerr, method)
		assert.IsType(t, want, got, method)
	}
}

// --- System.Check ------------------------------------------------------------

func (e *sysEnv) check(deep bool) []api.SystemFinding {
	e.t.Helper()
	return e.s.systemCheck(api.SystemCheckParams{Deep: deep})
}

func worst(findings []api.SystemFinding, check string) api.SystemFinding {
	rank := map[string]int{api.SeverityOK: 0, api.SeverityInfo: 1, api.SeverityWarning: 2, api.SeverityError: 3}
	best := api.SystemFinding{Severity: "none"}
	for _, f := range findings {
		if f.Check == check && (best.Severity == "none" || rank[f.Severity] > rank[best.Severity]) {
			best = f
		}
	}
	return best
}

func messages(findings []api.SystemFinding, check string) string {
	var out []string
	for _, f := range findings {
		if f.Check == check {
			out = append(out, f.Severity+": "+f.Message)
		}
	}
	return strings.Join(out, "\n")
}

func TestSystemCheck_HealthyDaemonIsAllOK(t *testing.T) {
	e := newSysEnv(t)
	disk := e.image("web", "latest", "web disk")
	vol := e.volume("data")
	_, err := e.s.netStore.Create("appnet", "10.81.0.0/24", "")
	require.NoError(t, err)
	e.vm("app", running, vm.Config{ImagePath: disk, NetworkName: "appnet", Volumes: []vm.VolumeMount{{DiskPath: vol.DiskPath}}})
	require.NoError(t, os.WriteFile(filepath.Join(e.root, "tools", "kernel.img"), []byte("k"), 0o600))
	e.s.SetHypervisor(api.DaemonHypervisor{Name: hypervisorpkg.MicroVM, Reason: "this host can run it", FirecrackerAvailable: true})

	for _, deep := range []bool{false, true} {
		findings := e.check(deep)
		var checks []string
		for _, f := range findings {
			assert.Equal(t, api.SeverityOK, f.Severity, "%s: %s", f.Check, f.Message)
			assert.Empty(t, f.Hint)
			assert.NotEmpty(t, f.Message)
			checks = append(checks, f.Check)
		}
		assert.Equal(t, []string{"hypervisor", "disk space", "images", "vms", "volumes", "networks", "kernel"}, checks)
	}
	assert.Contains(t, messages(e.check(true), "images"), "verified against their digests")
}

func TestSystemCheck_Hypervisor(t *testing.T) {
	e := newSysEnv(t)
	e.s.SetHypervisor(api.DaemonHypervisor{Name: "qemu", Reason: "chosen in config.toml", FirecrackerAvailable: true})
	f := worst(e.check(false), "hypervisor")
	assert.Equal(t, api.SeverityInfo, f.Severity)
	assert.Contains(t, f.Message, "can run "+hypervisorpkg.Label)
	assert.Contains(t, f.Hint, "hypervisor auto")

	e.s.SetHypervisor(api.DaemonHypervisor{Name: "qemu", Reason: "no KVM"})
	assert.Equal(t, api.SeverityOK, worst(e.check(false), "hypervisor").Severity, "QEMU is right where the microVM backend cannot run")
}

func TestSystemCheck_DiskSpace(t *testing.T) {
	e := newSysEnv(t)
	orig := filesystemSpace
	t.Cleanup(func() { filesystemSpace = orig })
	for _, tc := range []struct {
		free     int64
		severity string
	}{
		{free: 200 << 30, severity: api.SeverityOK},
		{free: lowSpaceWarn, severity: api.SeverityOK},
		{free: lowSpaceWarn - 1, severity: api.SeverityWarning},
		{free: lowSpaceError, severity: api.SeverityWarning},
		{free: lowSpaceError - 1, severity: api.SeverityError},
		{free: 0, severity: api.SeverityError},
	} {
		filesystemSpace = func(string) (int64, int64) { return 500 << 30, tc.free }
		f := worst(e.check(false), "disk space")
		assert.Equal(t, tc.severity, f.Severity, "free=%d", tc.free)
		if tc.severity != api.SeverityOK {
			assert.Contains(t, f.Hint, "system prune")
		}
	}
	filesystemSpace = func(string) (int64, int64) { return 0, 0 }
	f := worst(e.check(false), "disk space")
	assert.Equal(t, api.SeverityOK, f.Severity, "a filesystem that cannot be measured is not reported as full")
}

func TestSystemCheck_Images(t *testing.T) {
	t.Run("name of a missing disk", func(t *testing.T) {
		e := newSysEnv(t)
		gone := e.image("gone", "latest", "gone disk")
		require.NoError(t, os.RemoveAll(filepath.Dir(gone)))
		f := worst(e.check(false), "images")
		assert.Equal(t, api.SeverityWarning, f.Severity)
		assert.Contains(t, f.Message, "gone:latest")
		assert.Contains(t, f.Hint, "system prune")
	})
	t.Run("unreadable manifest", func(t *testing.T) {
		e := newSysEnv(t)
		disk := e.image("web", "latest", "web disk")
		tags, err := os.ReadDir(filepath.Join(filepath.Dir(disk), "tags"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(disk), "tags", tags[0].Name()), []byte("{"), 0o644))
		f := worst(e.check(false), "images")
		assert.Equal(t, api.SeverityError, f.Severity)
		assert.Contains(t, f.Hint, "jerboa rmi web:latest")
	})
	t.Run("modified disk is found only by a deep check", func(t *testing.T) {
		e := newSysEnv(t)
		disk := e.image("web", "latest", "web disk")
		_, err := e.s.imgStore.Tag("web:latest", "web", "v1")
		require.NoError(t, err)
		require.NoError(t, os.Chmod(disk, 0o644))
		require.NoError(t, os.WriteFile(disk, []byte("WEB disk"), 0o644))

		assert.Equal(t, api.SeverityOK, worst(e.check(false), "images").Severity, "the quick check does not read disks")
		deep := e.check(true)
		assert.Equal(t, api.SeverityError, worst(deep, "images").Severity)
		assert.Equal(t, 2, strings.Count(messages(deep, "images"), "is corrupt"), "both names of the damaged disk are reported")
	})
	t.Run("rebuilt index and unnamed disks are information", func(t *testing.T) {
		e := newSysEnv(t)
		e.image("web", "latest", "web disk")
		e.untagged("an old build")
		require.NoError(t, os.WriteFile(filepath.Join(e.root, "images", "refs.json.corrupt"), []byte("{"), 0o600))
		got := messages(e.check(false), "images")
		assert.Contains(t, got, "info: the image index was found damaged and was rebuilt")
		assert.Contains(t, got, "info: 1 stored image without a name")
		assert.Equal(t, api.SeverityInfo, worst(e.check(false), "images").Severity)
	})
	t.Run("an unnamed disk a VM boots from is not reclaimable", func(t *testing.T) {
		e := newSysEnv(t)
		dir := e.untagged("an old build")
		e.vm("app", stopped, vm.Config{ImagePath: filepath.Join(dir, "disk.img")})
		assert.Equal(t, api.SeverityOK, worst(e.check(false), "images").Severity)
	})
	t.Run("no image store", func(t *testing.T) {
		e := newSysEnv(t)
		e.s.imgStore = nil
		assert.Equal(t, api.SeverityError, worst(e.check(false), "images").Severity)
	})
}

func TestSystemCheck_VMs(t *testing.T) {
	t.Run("image gone", func(t *testing.T) {
		e := newSysEnv(t)
		gone := filepath.Join(e.root, "images", sha("x"), "disk.img")
		e.vm("orphaned", stopped, vm.Config{ImagePath: gone})
		e.vm("still-up", running, vm.Config{ImagePath: gone})
		got := messages(e.check(false), "vms")
		assert.Contains(t, got, "warning: VM orphaned cannot start again")
		assert.NotContains(t, got, "still-up", "a running VM already has its own boot disk")
		assert.Contains(t, worst(e.check(false), "vms").Hint, "jerboa rm orphaned")
	})
	t.Run("volume disk gone", func(t *testing.T) {
		e := newSysEnv(t)
		vol := e.volume("data")
		v := e.vm("", stopped, vm.Config{Volumes: []vm.VolumeMount{{DiskPath: vol.DiskPath, GuestPath: "/var/lib/db"}}})
		require.NoError(t, os.Remove(vol.DiskPath))
		f := worst(e.check(false), "vms")
		assert.Equal(t, api.SeverityError, f.Severity)
		assert.Contains(t, f.Message, v.ID, "a VM without a name is named by its id")
		assert.Contains(t, f.Message, "/var/lib/db")
	})
	t.Run("records and disks that belong to no VM", func(t *testing.T) {
		e := newSysEnv(t)
		e.vm("app", running, vm.Config{})
		require.NoError(t, os.MkdirAll(filepath.Join(e.root, "vms", "deadbeef"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(e.root, "vm-disks", "fc-gone-rootfs.img"), []byte("x"), 0o600))
		got := messages(e.check(false), "vms")
		assert.Contains(t, got, "warning: 1 VM record could not be read back")
		assert.Contains(t, got, "info: 1 boot disk left by VMs that are not running")
	})
}

func TestSystemCheck_Volumes(t *testing.T) {
	e := newSysEnv(t)
	e.volume("fine")
	short := e.volume("short")
	require.NoError(t, os.Truncate(short.DiskPath, 1<<20))
	nodisk := e.volume("nodisk")
	require.NoError(t, os.Remove(nodisk.DiskPath))
	require.NoError(t, os.Mkdir(filepath.Join(e.root, "volumes", "stray"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(e.root, "volumes", ".import-9"), 0o700))

	got := messages(e.check(false), "volumes")
	assert.Contains(t, got, "error: volume nodisk has no disk")
	assert.Contains(t, got, "warning: volume short is 256.0MB but its disk is only 1.0MB")
	assert.Contains(t, got, "warning: stray is in the volume store but is not a volume")
	assert.Contains(t, got, "info: 1 staging directory left by interrupted")
	assert.NotContains(t, got, "fine")
}

func TestSystemCheck_Networks(t *testing.T) {
	e := newSysEnv(t)
	_, err := e.s.netStore.Create("appnet", "10.82.0.0/24", "")
	require.NoError(t, err)
	e.vm("ok", running, vm.Config{NetworkName: "appnet"})
	e.vm("lost-a", stopped, vm.Config{NetworkName: "removed"})
	e.vm("lost-b", stopped, vm.Config{NetworkName: "removed"})
	e.vm("alone", running, vm.Config{})

	findings := e.check(false)
	f := worst(findings, "networks")
	assert.Equal(t, api.SeverityWarning, f.Severity)
	assert.Contains(t, f.Message, "network removed is gone but 2 VMs are still attached")
	assert.Contains(t, f.Message, "lost-a")
	assert.Contains(t, f.Message, "lost-b")
	assert.Equal(t, 1, strings.Count(messages(findings, "networks"), "warning"), "one finding per missing network")
}

type snapshotMock struct {
	*vm.MockManager
	entries []snapshot.Entry
	err     error
}

func (m *snapshotMock) SnapshotCreate(context.Context, string, string) (snapshot.Info, error) {
	return snapshot.Info{}, nil
}
func (m *snapshotMock) SnapshotRestore(context.Context, string, string) error { return nil }
func (m *snapshotMock) SnapshotList() ([]snapshot.Entry, error)               { return m.entries, m.err }
func (m *snapshotMock) SnapshotInspect(string) (snapshot.Info, error)         { return snapshot.Info{}, nil }
func (m *snapshotMock) SnapshotRemove(string) error                           { return nil }

func TestSystemCheck_Snapshots(t *testing.T) {
	e := newSysEnv(t)
	assert.Equal(t, "none", worst(e.check(false), "snapshots").Severity, "a backend without snapshots has nothing to check")

	sm := &snapshotMock{MockManager: e.mgr, entries: []snapshot.Entry{
		{Info: snapshot.Info{Name: "good"}},
		{Info: snapshot.Info{Name: "bad"}, Err: errors.New("artifact digest mismatch")},
	}}
	e.s.mgr = sm
	f := worst(e.check(false), "snapshots")
	assert.Equal(t, api.SeverityError, f.Severity)
	assert.Contains(t, f.Message, "snapshot bad cannot be used")
	assert.Contains(t, f.Hint, "jerboa snapshot rm bad")
	assert.Equal(t, 2, e.usage("snapshots").Count)

	sm.entries, sm.err = nil, errors.New("permission denied")
	assert.Equal(t, api.SeverityError, worst(e.check(false), "snapshots").Severity)

	sm.err = nil
	assert.Equal(t, api.SeverityOK, worst(e.check(false), "snapshots").Severity)
}

func TestSystemCheck_Kernel(t *testing.T) {
	e := newSysEnv(t)
	f := worst(e.check(false), "kernel")
	assert.Equal(t, api.SeverityInfo, f.Severity)
	assert.Contains(t, f.Message, "first build or run fetches it")

	e.s.paths.Tools = ""
	assert.Equal(t, "none", worst(e.check(false), "kernel").Severity)
}

func TestHandleSystemCheck_Params(t *testing.T) {
	e := newSysEnv(t)
	for _, raw := range []string{"", "null", "{}", `{"deep":true}`} {
		got, rerr := e.s.handleSystemCheck([]byte(raw))
		require.Nil(t, rerr, "params %q", raw)
		assert.NotEmpty(t, got.(api.SystemCheckResult).Findings)
	}
	_, rerr := e.s.handleSystemCheck([]byte(`{"deep":"very"}`))
	require.NotNil(t, rerr)
	assert.Equal(t, -32602, rerr.Code)
}

func TestHumanBytesAndPlural(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 1023: "1023B", 1024: "1.0KB", 5 << 20: "5.0MB", 3 << 30: "3.0GB", 2 << 40: "2048.0GB"} {
		assert.Equal(t, want, humanBytes(n))
	}
	assert.Equal(t, "1 image", plural(1, "image", "images"))
	assert.Equal(t, "0 images", plural(0, "image", "images"))
	assert.Equal(t, "2 images", plural(2, "image", "images"))
}
