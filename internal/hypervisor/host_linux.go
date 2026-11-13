package hypervisor

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// capNetAdmin is CAP_NET_ADMIN's bit in a capability set.
const capNetAdmin = 12

// unsupported says why this host cannot run Firecracker, or "". Firecracker
// needs KVM, and its guests' network devices need CAP_NET_ADMIN (the systemd
// unit grants it; root has it).
func unsupported() string {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return noKVM(err, os.IsNotExist(err) && underWSL())
	}
	_ = f.Close()
	if os.Geteuid() != 0 && !hasCapability(capNetAdmin) {
		return "the daemon may not create network devices (it needs CAP_NET_ADMIN)"
	}
	return ""
}

// hasCapability reports whether this process's effective set holds a capability.
func hasCapability(bit uint) bool {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if value, ok := strings.CutPrefix(s.Text(), "CapEff:"); ok {
			set, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
			return err == nil && set&(1<<bit) != 0
		}
	}
	return false
}

// noKVM says why KVM is out of reach. Inside WSL2 a missing /dev/kvm means
// Windows does not pass virtualization through, which the user can change.
func noKVM(err error, wsl bool) string {
	if wsl {
		return `KVM is not available: set nestedVirtualization=true under [wsl2] in %USERPROFILE%\.wslconfig, then run "wsl --shutdown"`
	}
	return "KVM is not available (/dev/kvm: " + err.Error() + ")"
}

// underWSL reports whether this kernel is a WSL2 one.
func underWSL() bool {
	version, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(version)), "microsoft")
}
