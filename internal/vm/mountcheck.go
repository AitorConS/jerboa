//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// mountCheckInterval and mountCheckTimeout bound the post-start scan of the
// guest serial log for a volume-mount failure. They are vars so tests can shrink
// them.
var (
	mountCheckInterval = 500 * time.Millisecond
	mountCheckTimeout  = 20 * time.Second
)

// detectMountFailure scans captured guest serial output for the kernel's
// volume-mount failure markers (kernel/src/kernel/storage.c). It returns the
// offending mount point and true on the first match.
//
// It exists to catch the silent-data-loss case: when a volume's mount point does
// not exist in the image rootfs, Nanos logs "storage: mount point <p> not found"
// and skips the mount, so the app writes to the ephemeral rootfs and the data
// vanishes on stop with no error surfaced anywhere. Matching the guest's own log
// makes that failure observable without ever flagging a VM whose mount succeeded.
func detectMountFailure(log []byte) (mountPoint string, failed bool) {
	s := string(log)
	for _, marker := range []string{
		"storage: mount point ",
		"storage: invalid mount point ",
	} {
		i := strings.Index(s, marker)
		if i < 0 {
			continue
		}
		rest := s[i+len(marker):]
		mp := rest
		if j := strings.IndexAny(mp, " \t\r\n"); j >= 0 {
			mp = mp[:j]
		}
		if mp != "" {
			return mp, true
		}
	}
	return "", false
}

// watchVolumeMounts scans v's captured serial output for a volume-mount failure
// and, on the first match, records a warning on the VM (surfaced by
// `jerboa inspect`) and logs it. It converts silent data loss — a volume that
// never mounts, so the app writes to ephemeral rootfs storage lost on stop —
// into a visible signal. Because it reacts only to a real failure message from
// the guest, it never flags a VM whose volumes mounted correctly.
//
// It is spawned only for VMs that actually attach a volume, and exits on the
// first detection, when the VM stops, on ctx cancellation, or after
// mountCheckTimeout — so it never outlives the boot window it is watching.
func watchVolumeMounts(ctx context.Context, v *VM) {
	defer recoverGoroutine("volume mount watch", v.ID)
	deadline := time.NewTimer(mountCheckTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(mountCheckInterval)
	defer ticker.Stop()
	for {
		if mp, failed := detectMountFailure(v.Logs()); failed {
			v.AddWarning(fmt.Sprintf(
				"volume mount point %q does not exist in the image: the volume did not mount and writes to it are lost when the VM stops — create the directory in the image (build.dirs in unikernel.toml)",
				mp))
			slog.Warn("vm: volume failed to mount in guest", "vm_id", v.ID, "mount_point", mp)
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return
		case <-v.Done():
			return
		case <-ctx.Done():
			return
		}
	}
}
