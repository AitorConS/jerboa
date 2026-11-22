package main

import (
	"runtime"

	"github.com/spf13/pflag"

	hypervisorpkg "github.com/AitorConS/jerboa/internal/hypervisor"
)

// microVMFlags registers the flags of the microVM backend under its platform
// name: --spark-* on macOS, where it is Spark, and --fc-* on Linux, where it
// is Firecracker. On macOS the --fc-* names that earlier releases wrote into
// launchd services still work, hidden.
func microVMFlags(fs *pflag.FlagSet, bin, security, kernel, metricsDir *string) {
	if runtime.GOOS == "darwin" {
		fs.StringVar(bin, "spark-bin", hypervisorpkg.DefaultBin, "Spark executable to use (only with --hypervisor=spark)")
		fs.StringVar(security, "spark-security", "", "Spark security policy JSON: the outbound and DNS permissions of guests, replacing the default (public addresses only)")
		fs.StringVar(kernel, "spark-kernel", "", "kernel Spark boots (default: tools-dir/kernel.img)")
		for _, legacy := range []struct {
			name, value string
			target      *string
		}{{"fc-bin", hypervisorpkg.DefaultBin, bin}, {"fc-security", "", security}, {"fc-kernel", "", kernel}} {
			fs.StringVar(legacy.target, legacy.name, legacy.value, "")
			_ = fs.MarkHidden(legacy.name)
		}
		// Linux only; registered so an invocation shared with Linux parses.
		fs.StringVar(metricsDir, "fc-metrics-dir", "", "")
		_ = fs.MarkHidden("fc-metrics-dir")
		return
	}
	fs.StringVar(bin, "fc-bin", hypervisorpkg.DefaultBin, "Firecracker binary to use (only with --hypervisor=firecracker)")
	fs.StringVar(security, "fc-security", "", "macOS only")
	_ = fs.MarkHidden("fc-security")
	fs.StringVar(metricsDir, "fc-metrics-dir", "",
		"write per-VM Firecracker device metrics (block I/O counts, flushes, latency) to this directory for profiling; empty disables")
	fs.StringVar(kernel, "fc-kernel", "", "Firecracker kernel (default: auto-downloaded)")
}
