//go:build linux

package vm

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// validateDiskIOHost rejects the async engine on kernels without a usable
// io_uring (Firecracker requires Linux 5.10+ for its Async block engine).
func validateDiskIOHost(cfg Config) error {
	if cfg.DiskIOEngine != DiskIOEngineAsync {
		return nil
	}
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return fmt.Errorf("disk I/O engine async: read kernel version: %w", err)
	}
	release := unix.ByteSliceToString(uts.Release[:])
	if !kernelAtLeast(release, 5, 10) {
		return fmt.Errorf("disk I/O engine async requires Linux 5.10 or newer (io_uring), host kernel is %s", release)
	}
	return nil
}

// kernelAtLeast reports whether a uname release such as "6.8.0-45-generic" is
// at least major.minor. Unparseable releases report false.
func kernelAtLeast(release string, major, minor int) bool {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minStr := parts[1]
	if i := strings.IndexFunc(minStr, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		minStr = minStr[:i]
	}
	mnr, err := strconv.Atoi(minStr)
	if err != nil {
		return false
	}
	return maj > major || (maj == major && mnr >= minor)
}
