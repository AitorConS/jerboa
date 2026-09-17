//go:build linux || (darwin && arm64)

package vm

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrepareBootDiskIntegrity(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "disks", "boot")
	original := []byte("verified image")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(original))
	require.NoError(t, os.WriteFile(src, original, 0o600))
	require.NoError(t, prepareBootDisk(dst, src, digest))
	data, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, original, data)
	// A source changed after reference resolution must never become bootable.
	require.NoError(t, os.WriteFile(src, []byte("tampered image"), 0o600))
	require.ErrorContains(t, prepareBootDisk(dst, src, digest), "integrity mismatch")
	_, err = os.Stat(dst)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPrepareBootDiskWithoutDigest(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "boot")
	require.NoError(t, os.WriteFile(src, []byte("unverified"), 0o600))
	require.NoError(t, prepareBootDisk(dst, src, ""))
	data, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, []byte("unverified"), data)
}

func TestBootDiskVMID(t *testing.T) {
	for name, want := range map[string]string{
		"qemu-abc123-boot.img":  "abc123",
		"fc-abc-123-rootfs.img": "abc-123",
	} {
		id, ok := bootDiskVMID(name)
		require.True(t, ok, name)
		require.Equal(t, want, id)
	}
	for _, name := range []string{"qemu--boot.img", "fc-vm-config.json", "disk.img", "fc-rootfs.img"} {
		_, ok := bootDiskVMID(name)
		require.False(t, ok, name)
	}
}

func TestSweepBootDisksKeepsLiveVMs(t *testing.T) {
	dir := t.TempDir()
	s := NewMemoryStore()
	running, err := s.Create(Config{Name: "running", ImagePath: "x", Memory: "64M"})
	require.NoError(t, err)
	running.State = StateRunning
	stopped, err := s.Create(Config{Name: "stopped", ImagePath: "x", Memory: "64M"})
	require.NoError(t, err)
	stopped.State = StateStopped

	keep := filepath.Join(dir, fcRootfsPrefix+running.ID+fcRootfsSuffix)
	orphans := []string{
		filepath.Join(dir, qemuBootDiskPrefix+stopped.ID+qemuBootDiskSuffix),
		filepath.Join(dir, fcRootfsPrefix+"gone"+fcRootfsSuffix),
	}
	unrelated := filepath.Join(dir, "notes.txt")
	for _, p := range append([]string{keep, unrelated}, orphans...) {
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	}

	SweepBootDisks(dir, s)

	require.FileExists(t, keep)
	require.FileExists(t, unrelated)
	for _, p := range orphans {
		require.NoFileExists(t, p)
	}
	SweepBootDisks("", s) // no dedicated dir: must be a no-op
	SweepBootDisks(filepath.Join(dir, "missing"), s)
}

func TestRootfsPathUsesDiskDir(t *testing.T) {
	dir := t.TempDir()
	fc := NewFirecrackerManager("firecracker", "vmlinux", WithFCDiskDir(dir))
	require.Equal(t, filepath.Join(dir, "fc-vm1-rootfs.img"), fc.rootfsPath("vm1"))
	q := NewQEMUManager("qemu", WithDiskDir(dir))
	require.Equal(t, filepath.Join(dir, "qemu-vm1-boot.img"), q.bootDiskPath("vm1"))
	require.Equal(t, filepath.Clean(os.TempDir()), filepath.Dir(NewFirecrackerManager("firecracker", "vmlinux").rootfsPath("vm1")))
}
