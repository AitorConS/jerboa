//go:build linux

package vm

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

var asyncIOProbe = sync.OnceValue(probeAsyncIO)

// defaultFCIOEngine is Firecracker's Async engine when the host can run it,
// else Sync. Firecracker fails to start a drive whose engine cannot be set
// up, so the default is chosen by probing rather than assumed.
func defaultFCIOEngine() string {
	if asyncIOProbe() {
		return fcEngineAsync
	}
	return fcEngineSync
}

// probeAsyncIO reports whether this process can create an io_uring on a
// kernel new enough for Firecracker's Async engine. It fails where io_uring
// is disabled (kernel.io_uring_disabled) or filtered (container seccomp).
func probeAsyncIO() bool {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil || !kernelAtLeast(unix.ByteSliceToString(uts.Release[:]), 5, 10) {
		return false
	}
	var params [120]byte // struct io_uring_params, zeroed
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 1, uintptr(unsafe.Pointer(&params[0])), 0)
	if errno != 0 {
		return false
	}
	unix.Close(int(fd))
	return true
}

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
