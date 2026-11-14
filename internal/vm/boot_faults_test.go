//go:build linux || (darwin && arm64)

package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Private boot disks when the image is missing, damaged or changes under the
// daemon, when the disk directory cannot be written, and when disks are left
// behind by a crash. A VM must never boot from bytes that were not verified,
// and a failed preparation must not leave a disk behind.

func bootImage(t *testing.T, dir string, content []byte) (path, digest string) {
	t.Helper()
	path = filepath.Join(dir, "image.img")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path, fmt.Sprintf("sha256:%x", sha256.Sum256(content))
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestFaults_BootDiskFromAnUnusableImageLeavesNothing(t *testing.T) {
	content := bytes.Repeat([]byte("image "), 4096)
	cases := map[string]func(t *testing.T, src string){
		"image deleted": func(t *testing.T, src string) { require.NoError(t, os.Remove(src)) },
		"image is a directory": func(t *testing.T, src string) {
			require.NoError(t, os.Remove(src))
			require.NoError(t, os.Mkdir(src, 0o700))
		},
		// A power cut left the image shorter than what was hashed.
		"image truncated": func(t *testing.T, src string) { require.NoError(t, os.Truncate(src, int64(len(content)/2))) },
		"image emptied":   func(t *testing.T, src string) { require.NoError(t, os.Truncate(src, 0)) },
		"image grown":     func(t *testing.T, src string) { require.NoError(t, os.Truncate(src, int64(len(content)+1))) },
		"one byte changed": func(t *testing.T, src string) {
			f, err := os.OpenFile(src, os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.WriteAt([]byte{'X'}, int64(len(content)-1))
			require.NoError(t, err)
			require.NoError(t, f.Close())
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			src, digest := bootImage(t, dir, content)
			fn(t, src)
			disks := filepath.Join(dir, "disks")
			dst := filepath.Join(disks, "qemu-abc-boot.img")

			require.Error(t, prepareBootDisk(dst, src, digest, 1<<20, nil))
			assert.Empty(t, dirNames(t, disks), "a boot disk that failed verification was left on disk")

			// A trust function that does not vouch for the file changes nothing.
			require.Error(t, prepareBootDisk(dst, src, digest, 0, func(string, string) bool { return false }))
			assert.Empty(t, dirNames(t, disks))
		})
	}
}

func TestFaults_BootDiskReplacesALeftoverFromACrashedRun(t *testing.T) {
	dir := t.TempDir()
	content := []byte("verified image")
	src, digest := bootImage(t, dir, content)
	dst := filepath.Join(dir, "disks", "qemu-abc-boot.img")
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o700))
	// The previous run of this VM died and left its (modified, longer) disk.
	require.NoError(t, os.WriteFile(dst, bytes.Repeat([]byte("guest wrote this "), 100), 0o600))

	require.NoError(t, prepareBootDisk(dst, src, digest, 0, nil))
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, content, got, "the VM would boot from what the previous run left behind")
	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestFaults_BootDiskDirectoryThatCannotBeUsed(t *testing.T) {
	content := []byte("verified image")
	t.Run("parent is a file", func(t *testing.T) {
		dir := t.TempDir()
		src, digest := bootImage(t, dir, content)
		blocker := filepath.Join(dir, "disks")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
		require.Error(t, prepareBootDisk(filepath.Join(blocker, "qemu-abc-boot.img"), src, digest, 0, nil))
	})
	t.Run("read-only", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := t.TempDir()
		src, digest := bootImage(t, dir, content)
		disks := filepath.Join(dir, "disks")
		require.NoError(t, os.Mkdir(disks, 0o500))
		t.Cleanup(func() { _ = os.Chmod(disks, 0o700) })
		require.Error(t, prepareBootDisk(filepath.Join(disks, "qemu-abc-boot.img"), src, digest, 0, nil))
		assert.Empty(t, dirNames(t, disks))
	})
	t.Run("destination is a directory", func(t *testing.T) {
		dir := t.TempDir()
		src, digest := bootImage(t, dir, content)
		dst := filepath.Join(dir, "disks", "qemu-abc-boot.img")
		require.NoError(t, os.MkdirAll(filepath.Join(dst, "sub"), 0o700))
		require.Error(t, prepareBootDisk(dst, src, digest, 0, nil))
	})
}

func TestFaults_BootDiskTrustIsCheckedAfterTheCopy(t *testing.T) {
	dir := t.TempDir()
	content := []byte("verified image")
	src, digest := bootImage(t, dir, content)
	dst := filepath.Join(dir, "disks", "qemu-abc-boot.img")
	// The image is replaced while it is being copied: the store withdraws its
	// trust, so the copy has to be hashed, and the hash does not match.
	trusted := func(path, askedDigest string) bool {
		assert.Equal(t, src, path)
		assert.Equal(t, digest, askedDigest)
		_, err := os.Stat(dst)
		assert.NoError(t, err, "trust was asked before the copy existed")
		return false
	}
	require.NoError(t, os.WriteFile(src, []byte("replaced image"), 0o600))
	require.ErrorContains(t, prepareBootDisk(dst, src, digest, 0, trusted), "integrity mismatch")
	assert.NoFileExists(t, dst)
}

func TestFaults_ExtendDiskBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	size := func() int64 {
		info, err := os.Stat(path)
		require.NoError(t, err)
		return info.Size()
	}
	require.NoError(t, os.WriteFile(path, make([]byte, 1000), 0o600))

	for _, min := range []int64{0, -5, 1, 999, 1000} {
		require.NoError(t, extendDisk(path, min))
		assert.Equal(t, int64(1000), size(), "minSize %d must leave a large enough disk alone", min)
	}
	require.NoError(t, extendDisk(path, 1001))
	assert.Equal(t, int64(diskSizeAlign), size(), "one byte short rounds up to the alignment")
	require.NoError(t, extendDisk(path, diskSizeAlign))
	assert.Equal(t, int64(diskSizeAlign), size())
	require.NoError(t, extendDisk(path, diskSizeAlign+1))
	assert.Equal(t, int64(2*diskSizeAlign), size())
	// Extending never shrinks and never touches the existing bytes.
	require.NoError(t, extendDisk(path, 10))
	assert.Equal(t, int64(2*diskSizeAlign), size())

	require.Error(t, extendDisk(filepath.Join(t.TempDir(), "missing.img"), 10))
	require.NoError(t, extendDisk(filepath.Join(t.TempDir(), "missing.img"), 0))
}

func TestFaults_StartWithAnUnverifiableImageLaunchesNothing(t *testing.T) {
	content := bytes.Repeat([]byte("image "), 1024)
	cases := map[string]func(t *testing.T, src string){
		"image deleted":   func(t *testing.T, src string) { require.NoError(t, os.Remove(src)) },
		"image truncated": func(t *testing.T, src string) { require.NoError(t, os.Truncate(src, 100)) },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			src, digest := bootImage(t, dir, content)
			disks := filepath.Join(dir, "disks")
			spawns := always("block")
			mgr := NewQEMUManager("fake-qemu", WithCommandFunc(spawns.cmd), WithDiskDir(disks))
			cfg := faultCfg()
			cfg.ImagePath = src
			cfg.ImageDigest = digest
			cfg.Restart = RestartConfig{Policy: RestartAlways}
			v, err := mgr.Create(context.Background(), cfg)
			require.NoError(t, err)
			fn(t, src)

			require.Error(t, mgr.Start(context.Background(), v.ID))
			assert.Zero(t, spawns.n.Load(), "a hypervisor was launched on an image that failed verification")
			assert.Equal(t, StateStopped, v.GetState())
			waitDone(t, v)
			assert.Empty(t, dirNames(t, disks))
			require.NoError(t, mgr.Remove(context.Background(), v.ID))
		})
	}
}

func TestFaults_PrivateBootDiskLivesExactlyAsLongAsTheVM(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("image "), 1024)
	src, digest := bootImage(t, dir, content)
	disks := filepath.Join(dir, "disks")
	mgr := NewQEMUManager("fake-qemu", WithCommandFunc(fakeVMM("block")), WithDiskDir(disks))
	cfg := faultCfg()
	cfg.ImagePath = src
	cfg.ImageDigest = digest
	v, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))

	disk := mgr.bootDiskPath(v.ID)
	got, err := os.ReadFile(disk)
	require.NoError(t, err)
	assert.Equal(t, content, got)
	// A sweep while the VM runs (a second daemon start) must not take its disk.
	SweepBootDisks(disks, mgr.Store())
	assert.FileExists(t, disk)
	assert.Empty(t, OrphanBootDisks(disks, mgr.List()))

	require.NoError(t, mgr.Kill(context.Background(), v.ID))
	waitDone(t, v)
	require.Eventually(t, func() bool {
		_, err := os.Stat(disk)
		return os.IsNotExist(err)
	}, 10*time.Second, 5*time.Millisecond, "the private boot disk outlived its VM")
	// The shared image was never written to.
	base, err := os.ReadFile(src)
	require.NoError(t, err)
	assert.Equal(t, content, base)
}

func TestFaults_SweepRemovesExactlyTheOrphans(t *testing.T) {
	dir := t.TempDir()
	s := NewMemoryStore()
	want := map[string]bool{} // file name -> must survive the sweep
	for _, state := range []State{StateCreated, StateStarting, StateRunning, StateStopping, StateStopped, StateRestoring} {
		v, err := s.Create(Config{Name: string(state), ImagePath: "x"})
		require.NoError(t, err)
		v.mu.Lock()
		v.State = state
		v.mu.Unlock()
		live := state != StateCreated && state != StateStopped
		want[qemuBootDiskPrefix+v.ID+qemuBootDiskSuffix] = live
		want[fcRootfsPrefix+v.ID+fcRootfsSuffix] = live
	}
	// Disks of VMs the registry no longer knows, e.g. removed while down.
	want[qemuBootDiskPrefix+"deadbeef0000"+qemuBootDiskSuffix] = false
	want[fcRootfsPrefix+"deadbeef0000"+fcRootfsSuffix] = false
	// Things that are not boot disks must be left alone.
	for _, other := range []string{"notes.txt", "qemu-boot.img", "fc-abc-config.json", ".qemu-abc-boot.img.tmp"} {
		want[other] = true
	}
	for name := range want {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("disk"), 0o600))
	}
	// A directory that happens to be named like a disk.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "qemu-dir-boot.img"), 0o700))

	var wantOrphans, wantKept []string
	for name, keep := range want {
		if keep {
			wantKept = append(wantKept, name)
		} else {
			wantOrphans = append(wantOrphans, filepath.Join(dir, name))
		}
	}
	wantKept = append(wantKept, "qemu-dir-boot.img")
	sort.Strings(wantKept)

	// The dry-run listing and the sweep must agree.
	assert.ElementsMatch(t, wantOrphans, OrphanBootDisks(dir, s.List()))
	SweepBootDisks(dir, s)
	assert.Equal(t, wantKept, dirNames(t, dir))
	assert.Empty(t, OrphanBootDisks(dir, s.List()))
	SweepBootDisks(dir, s) // idempotent
	assert.Equal(t, wantKept, dirNames(t, dir))
}

func TestFaults_SweepOfADirectoryItCannotUse(t *testing.T) {
	s := NewMemoryStore()
	// None of these may panic or remove anything.
	SweepBootDisks("", s)
	assert.Nil(t, OrphanBootDisks("", nil))
	missing := filepath.Join(t.TempDir(), "never-created")
	SweepBootDisks(missing, s)
	assert.Nil(t, OrphanBootDisks(missing, nil))
	assert.NoDirExists(t, missing, "a sweep must not create the directory")
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	SweepBootDisks(file, s)
	assert.Nil(t, OrphanBootDisks(file, nil))
	assert.FileExists(t, file)

	if os.Geteuid() == 0 {
		return
	}
	// Orphans that cannot be deleted are reported again next time rather
	// than aborting the sweep.
	dir := t.TempDir()
	for _, id := range []string{"aaaa", "bbbb"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, qemuBootDiskPrefix+id+qemuBootDiskSuffix), nil, 0o600))
	}
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	SweepBootDisks(dir, s)
	assert.Len(t, OrphanBootDisks(dir, nil), 2)
}
