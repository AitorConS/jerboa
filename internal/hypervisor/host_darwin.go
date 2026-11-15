package hypervisor

import (
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// unsupported says why this Mac cannot run the native Firecracker, or "": it
// needs Apple Silicon and macOS 26.
func unsupported() string {
	if runtime.GOARCH != "arm64" {
		return "native Firecracker needs Apple Silicon"
	}
	version, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return "the macOS version is unknown"
	}
	major, _, _ := strings.Cut(version, ".")
	if n, err := strconv.Atoi(major); err != nil || n < 26 {
		return "native Firecracker needs macOS 26 (this is " + version + ")"
	}
	return ""
}
