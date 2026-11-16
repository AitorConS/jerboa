//go:build e2e && linux

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

var netAdmin struct {
	once sync.Once
	err  error
}

// netAdminErr reports why this process may not create network devices, or
// nil when it may.
func netAdminErr() error {
	netAdmin.once.Do(func() {
		probe := fmt.Sprintf("jbe2e-p%d", os.Getpid()%10000)
		if out, err := exec.Command("ip", "link", "add", probe, "type", "dummy").CombinedOutput(); err != nil {
			netAdmin.err = fmt.Errorf("ip link add: %w: %s", err, out)
			return
		}
		_ = exec.Command("ip", "link", "del", probe).Run()
	})
	return netAdmin.err
}

// grantNetAdmin is how a runner gets the capabilities guest networking
// needs.
const grantNetAdmin = "give the runner service the capabilities (systemctl edit <runner service>: [Service] AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW, then restart it), " +
	"or let the runner user use sudo without a password (scripts/run-e2e.sh then runs the tests under sudo): echo \"$USER ALL=(ALL) NOPASSWD: ALL\" | sudo tee /etc/sudoers.d/jerboa-e2e"

// requireGuestNetworking makes sure this process, and so the daemons it
// starts, may create network devices: on Linux every guest network is a
// bridge with a tap per VM, which needs CAP_NET_ADMIN. The suite gets it by
// running as root or from a runner service that holds it as an ambient
// capability (as the jerboad systemd unit does).
func requireGuestNetworking(t *testing.T) {
	t.Helper()
	if err := netAdminErr(); err != nil {
		missing(t, "guest networking needs CAP_NET_ADMIN (%v); %s", err, grantNetAdmin)
	}
}

// platformPreflight checks what the suite needs of a Linux host.
func platformPreflight() []lack {
	var out []lack
	tool := func(name, why, fix string) bool {
		if _, err := exec.LookPath(name); err != nil {
			out = append(out, lack{name + " is not in PATH (" + why + ")", fix})
			return false
		}
		return true
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		out = append(out, lack{fmt.Sprintf("/dev/kvm cannot be opened (%v)", err), "sudo usermod -aG kvm \"$USER\" and restart the runner; on a VM, enable nested virtualization"})
	} else {
		_ = f.Close()
	}
	tool("qemu-system-x86_64", "the QEMU backend", "sudo apt-get install -y qemu-system-x86")
	tool("firecracker", "the Firecracker backend", "install the firecracker release binary into /usr/local/bin (scripts/install.sh pins the version and its checksum)")
	tool("gcc", "the kernel boot test compiles its guest", "sudo apt-get install -y gcc")
	if tool("bwrap", "the build sandbox", "sudo apt-get install -y bubblewrap") {
		if msg, err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-net", "--", "true").CombinedOutput(); err != nil {
			out = append(out, lack{fmt.Sprintf("bubblewrap cannot create a sandbox (%v: %s)", err, msg),
				"allow unprivileged user namespaces: sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 (Ubuntu 24.04) or kernel.unprivileged_userns_clone=1, and persist it in /etc/sysctl.d/"})
		}
	}
	tool("nft", "the loopback guard and kernel NAT", "sudo apt-get install -y nftables")
	hasIP := tool("ip", "bridges and taps", "sudo apt-get install -y iproute2")
	if hasIP {
		if err := netAdminErr(); err != nil {
			out = append(out, lack{fmt.Sprintf("no CAP_NET_ADMIN: guest networks cannot be created (%v)", err), grantNetAdmin})
		}
	}
	if fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL))); err != nil {
		out = append(out, lack{fmt.Sprintf("no CAP_NET_RAW: the loopback guard test cannot inject frames (%v)", err), grantNetAdmin})
	} else {
		_ = unix.Close(fd)
	}
	for _, f := range []string{"tools/bin/mkfs", "platform/pc/bin/kernel.img", "platform/pc/boot/boot.img", "platform/pc/bin/kernel.elf"} {
		if suite.tools != "" && f != "platform/pc/bin/kernel.elf" {
			continue
		}
		if _, err := os.Stat(suite.repo + "/kernel/output/" + f); err != nil {
			out = append(out, lack{"kernel artifact kernel/output/" + f + " is missing", "make kernel && make -C kernel tools"})
		}
	}
	if os.Getenv("JERBOA_E2E_SLOW") != "" && os.Geteuid() != 0 {
		if _, err := exec.Command("sudo", "-n", "true").CombinedOutput(); err != nil {
			out = append(out, lack{"not root: the full-disk scenario cannot mount a small filesystem", "run the suite as root, or give the runner user passwordless sudo (see above)"})
		}
	}
	return out
}
