//go:build linux || (darwin && arm64)

package vm

import "fmt"

// Disk I/O engines (Config.DiskIOEngine).
const (
	// DiskIOEngineSync performs blocking host I/O on the VMM's device thread,
	// one request at a time. It has no kernel requirements.
	DiskIOEngineSync = "sync"
	// DiskIOEngineAsync submits host I/O through io_uring (Linux 5.10+), so
	// independent requests overlap on the host. Firecracker uses it by default
	// on Linux hosts where io_uring works (measured on a KVM host: random 4K
	// reads 4.9x, random writes +7%); elsewhere the default is sync.
	DiskIOEngineAsync = "async"
)

// Volume cache modes (Config.VolumeCache).
const (
	// VolumeCacheWriteback forwards guest flush requests to host storage, so
	// an fsync inside the guest (e.g. a database commit) survives a host crash.
	// It is the default for volumes on every hypervisor.
	VolumeCacheWriteback = "writeback"
	// VolumeCacheUnsafe ignores guest flushes. Writes are faster, but data the
	// guest believed durable can be lost if the host crashes or loses power.
	VolumeCacheUnsafe = "unsafe"
)

// validateDiskIO checks the hypervisor-neutral disk I/O options.
func validateDiskIO(cfg Config) error {
	switch cfg.DiskIOEngine {
	case "", DiskIOEngineSync, DiskIOEngineAsync:
	default:
		return fmt.Errorf("validate config: DiskIOEngine %q is invalid (want %s or %s)", cfg.DiskIOEngine, DiskIOEngineSync, DiskIOEngineAsync)
	}
	switch cfg.VolumeCache {
	case "", VolumeCacheWriteback, VolumeCacheUnsafe:
	default:
		return fmt.Errorf("validate config: VolumeCache %q is invalid (want %s or %s)", cfg.VolumeCache, VolumeCacheWriteback, VolumeCacheUnsafe)
	}
	return nil
}

// qemuDriveIOOpts returns the -drive suffix selecting the host I/O engine.
// QEMU's default (threads) is the counterpart of Firecracker's Sync engine.
func qemuDriveIOOpts(cfg Config) string {
	if cfg.DiskIOEngine == DiskIOEngineAsync {
		return ",aio=io_uring"
	}
	return ""
}

// qemuVolumeCacheOpts returns the -drive suffix for a volume's cache mode.
// QEMU's default (cache=writeback) already honors guest flushes.
func qemuVolumeCacheOpts(cfg Config) string {
	if cfg.VolumeCache == VolumeCacheUnsafe {
		return ",cache=unsafe"
	}
	return ""
}

// Firecracker drive option values (API names).
const (
	fcCacheUnsafe    = "Unsafe"
	fcCacheWriteback = "Writeback"
	fcEngineSync     = "Sync"
	fcEngineAsync    = "Async"
)

// fcIOEngine maps Config.DiskIOEngine to Firecracker's io_engine value.
// An empty DiskIOEngine selects the host default (defaultFCIOEngine).
func fcIOEngine(cfg Config) string {
	switch cfg.DiskIOEngine {
	case DiskIOEngineAsync:
		return fcEngineAsync
	case DiskIOEngineSync:
		return fcEngineSync
	}
	return defaultFCIOEngine()
}

// fcVolumeCacheType maps Config.VolumeCache to Firecracker's cache_type.
// Firecracker defaults to Unsafe, which silently drops guest flushes; volumes
// default to Writeback so durability matches QEMU.
func fcVolumeCacheType(cfg Config) string {
	if cfg.VolumeCache == VolumeCacheUnsafe {
		return fcCacheUnsafe
	}
	return fcCacheWriteback
}

// fcRootRateLimiter translates the boot-disk throttle into a Firecracker token
// bucket rate limiter (1 s refill), or nil when no limit is set.
func fcRootRateLimiter(cfg Config) *fcRateLimiter {
	if cfg.DiskIOPS == 0 && cfg.DiskBPS <= 0 {
		return nil
	}
	rl := &fcRateLimiter{}
	if cfg.DiskBPS > 0 {
		rl.Bandwidth = &fcTokenBucket{Size: cfg.DiskBPS, RefillTime: 1000}
	}
	if cfg.DiskIOPS > 0 {
		rl.Ops = &fcTokenBucket{Size: int64(cfg.DiskIOPS), RefillTime: 1000}
	}
	return rl
}
