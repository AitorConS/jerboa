//go:build linux

package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCopyFileIndependentCopy verifies copyFile produces a byte-identical but
// independent file: writing to the destination must not affect the source. This
// is the property that lets several Firecracker VMs share a base image safely.
func TestCopyFileIndependentCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "base.img")
	dst := filepath.Join(dir, "vm-rootfs.img")
	want := []byte("unikernel base image contents")
	require.NoError(t, os.WriteFile(src, want, 0o600))

	require.NoError(t, copyFile(dst, src))

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, want, got)

	// Mutating the copy must leave the base pristine.
	require.NoError(t, os.WriteFile(dst, []byte("guest writes"), 0o600))
	base, err := os.ReadFile(src)
	require.NoError(t, err)
	require.Equal(t, want, base, "base image must not change when the copy is written")
}

// TestWriteFCConfigUsesRootfsCopy verifies the generated Firecracker config
// points the root drive at the per-VM copy, not the shared store image, so
// concurrent VMs never write to the same backing file.
func TestWriteFCConfigUsesRootfsCopy(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "vmlinux")
	cfg := Config{ImagePath: "/store/images/app.img", Memory: "256M", CPUs: 1}
	rootfs := filepath.Join(t.TempDir(), "fc-test-rootfs.img")

	cfgPath, err := m.writeFCConfig("test-vm", cfg, rootfs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(cfgPath) })

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	var parsed fcVMConfig
	require.NoError(t, json.Unmarshal(data, &parsed))

	require.NotEmpty(t, parsed.Drives)
	root := parsed.Drives[0]
	require.Equal(t, "rootfs", root.DriveID)
	require.True(t, root.IsRootDevice)
	require.Equal(t, rootfs, root.PathOnHost, "root drive must use the per-VM copy")
	require.NotEqual(t, cfg.ImagePath, root.PathOnHost, "root drive must not use the shared store image")
}
