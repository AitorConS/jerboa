//go:build linux || (darwin && arm64)

package vm

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/AitorConS/jerboa/internal/diskclone"
)

// prepareBootDisk creates dst as the VM's private, writable boot disk from the
// shared store image at src. It uses a copy-on-write clone when the filesystem
// supports one and a hole-preserving copy otherwise, so starting a VM no longer
// writes the whole image.
//
// When digest is set, the private disk itself is hashed after it is created.
// A clone is a point-in-time copy, so this verifies exactly the bytes the VM
// boots from, even if the source changed during a fallback copy. On any error
// dst is removed.
func prepareBootDisk(dst, src, digest string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create boot disk dir: %w", err)
	}
	method, err := diskclone.Clone(dst, src)
	if err != nil {
		return fmt.Errorf("create boot disk: %w", err)
	}
	if digest != "" {
		got, err := diskclone.HashFile(dst)
		if err != nil {
			_ = os.Remove(dst)
			return fmt.Errorf("verify boot disk: %w", err)
		}
		if got != digest {
			_ = os.Remove(dst)
			return fmt.Errorf("boot image integrity mismatch")
		}
	}
	slog.Debug("vm: prepared private boot disk", "path", dst, "method", method)
	return nil
}

// Private boot disk file names. The VM ID is embedded so a sweep can match a
// leftover disk to its VM.
const (
	qemuBootDiskPrefix = "qemu-"
	qemuBootDiskSuffix = "-boot.img"
	fcRootfsPrefix     = "fc-"
	fcRootfsSuffix     = "-rootfs.img"
)

// bootDiskDir returns the directory for private boot disks. Empty keeps the
// historical system temp dir. A dedicated directory on the image store's
// filesystem lets prepareBootDisk clone instead of copy, and keeps large disks
// off a RAM-backed /tmp.
func bootDiskDir(dir string) string {
	if dir == "" {
		return os.TempDir()
	}
	return dir
}

// SweepBootDisks removes private boot disks in dir left behind by VMs that are
// no longer running, e.g. after a daemon crash or a re-adopted VM that exited
// while the daemon was down. Only call it for a dedicated directory: sweeping a
// shared temp dir could remove disks owned by another daemon.
func SweepBootDisks(dir string, s Store) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("vm: sweep boot disks", "dir", dir, "err", err)
		}
		return
	}
	live := make(map[string]bool)
	for _, v := range s.List() {
		switch v.GetState() {
		case StateStopped, StateCreated:
		default:
			live[v.ID] = true
		}
	}
	for _, e := range entries {
		id, ok := bootDiskVMID(e.Name())
		if !ok || e.IsDir() || live[id] {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.Remove(p); err != nil {
			slog.Warn("vm: remove orphaned boot disk", "path", p, "err", err)
			continue
		}
		slog.Info("vm: removed orphaned boot disk", "path", p, "vm_id", id)
	}
}

// bootDiskVMID extracts the VM ID from a private boot disk file name.
func bootDiskVMID(name string) (string, bool) {
	for _, affix := range [][2]string{{qemuBootDiskPrefix, qemuBootDiskSuffix}, {fcRootfsPrefix, fcRootfsSuffix}} {
		if strings.HasPrefix(name, affix[0]) && strings.HasSuffix(name, affix[1]) && len(name) > len(affix[0])+len(affix[1]) {
			return name[len(affix[0]) : len(name)-len(affix[1])], true
		}
	}
	return "", false
}
