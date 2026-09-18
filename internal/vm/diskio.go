//go:build linux || (darwin && arm64)

package vm

import "fmt"

// Disk I/O engines (Config.DiskIOEngine).
const (
	// DiskIOEngineSync performs blocking host I/O on the VMM's device thread.
	// It is the default: no kernel requirements and the lowest startup cost.
	DiskIOEngineSync = "sync"
	// DiskIOEngineAsync submits host I/O through io_uring (Linux 5.10+). It can
	// raise throughput for parallel or latency-sensitive workloads at the cost
	// of extra host CPU and slightly slower device setup, so it is opt-in.
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
func fcIOEngine(cfg Config) string {
	if cfg.DiskIOEngine == DiskIOEngineAsync {
		return fcEngineAsync
	}
	return fcEngineSync
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
