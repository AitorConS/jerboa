//go:build linux

package network

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/AitorConS/jerboa/internal/netconst"
)

// BridgeConfig holds the parameters for a Linux bridge interface.
type BridgeConfig struct {
	// Name is the bridge interface name (e.g. "jerboa-br0").
	Name string
	// CIDR is the gateway IP with CIDR mask (e.g. "10.0.0.1/24").
	CIDR string
}

// CreateBridge creates a Linux bridge, assigns the gateway IP, and brings it up.
// Requires CAP_NET_ADMIN.
func CreateBridge(cfg BridgeConfig) error {
	if err := ipLink("add", cfg.Name, "type", "bridge"); err != nil {
		return fmt.Errorf("create bridge %s: %w", cfg.Name, err)
	}
	if err := ipAddr("add", cfg.CIDR, "dev", cfg.Name); err != nil {
		_ = ipLink("del", cfg.Name)
		return fmt.Errorf("assign %s to bridge %s: %w", cfg.CIDR, cfg.Name, err)
	}
	if err := ipLink("set", cfg.Name, "up"); err != nil {
		_ = ipLink("del", cfg.Name)
		return fmt.Errorf("bring up bridge %s: %w", cfg.Name, err)
	}
	if err := enableForwarding(); err != nil {
		return fmt.Errorf("enable forwarding: %w", err)
	}
	return nil
}

// EnsureBridge creates the bridge if it does not already exist, assigning the
// gateway IP and bringing it up. Unlike CreateBridge it is idempotent, so it is
// safe to call for a bridge shared by multiple VMs.
//
// Only bridges in the jerboa namespace are managed: an existing interface is
// reused only when it is a bridge, so a guest is never attached to, and the
// MTU never changed on, an interface the daemon does not own.
func EnsureBridge(cfg BridgeConfig) error {
	if err := checkManagedBridgeName(cfg.Name); err != nil {
		return err
	}
	if err := checkBridgeCIDR(cfg.CIDR); err != nil {
		return fmt.Errorf("bridge %s: %w", cfg.Name, err)
	}
	if bridgeExists(cfg.Name) {
		if !isBridge(cfg.Name) {
			return fmt.Errorf("interface %s exists and is not a bridge", cfg.Name)
		}
		// Bridges created before jumbo frames keep working: raise their MTU.
		if err := setMTU(cfg.Name); err != nil {
			return err
		}
		return GuardBridge(cfg.Name)
	}
	if err := CreateBridge(cfg); err != nil {
		return err
	}
	if err := setMTU(cfg.Name); err != nil {
		return err
	}
	return GuardBridge(cfg.Name)
}

// checkBridgeCIDR applies the subnet rules of Store.Create again before an
// address is put on the host, so a network stored by an older version cannot
// claim a host route either: the gateway must be the subnet's first address.
func checkBridgeCIDR(cidr string) error {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parse %q: %w", cidr, err)
	}
	if err := checkSubnet(ipNet); err != nil {
		return fmt.Errorf("subnet %s %w", ipNet, err)
	}
	if !ip.Equal(firstHost(ipNet)) {
		return fmt.Errorf("gateway %s is not the first address of %s", ip, ipNet)
	}
	return nil
}

// managedBridgePrefix is the prefix of every bridge the daemon creates
// ("jerboa-br0" and the per-network "jerboa-br-<name>").
const managedBridgePrefix = "jerboa-br"

func checkManagedBridgeName(name string) error {
	if !strings.HasPrefix(name, managedBridgePrefix) || len(name) > maxIfaceNameLen ||
		strings.ContainsAny(name, "/. \t\n") {
		return fmt.Errorf("bridge %q is not managed by jerboa", name)
	}
	return nil
}

// isBridge reports whether the interface is a Linux bridge.
func isBridge(name string) bool {
	_, err := os.Stat("/sys/class/net/" + name + "/bridge")
	return err == nil
}

// setMTU sets netconst.BridgeMTU on a bridge or TAP.
func setMTU(name string) error {
	if err := ipLink("set", name, "mtu", strconv.Itoa(netconst.BridgeMTU)); err != nil {
		return fmt.Errorf("set mtu of %s: %w", name, err)
	}
	return nil
}

func bridgeExists(name string) bool {
	return exec.Command("ip", "link", "show", name).Run() == nil
}

// EnsureDNSAddress assigns the reserved guest-DNS address to the loopback
// interface so the daemon can answer DNS on it. Because it is a local address,
// packets sent by any bridged guest (routed via its default gateway) are
// delivered to the daemon regardless of which bridge they arrive on — a single
// address serves every network. Idempotent: an existing address is success.
func EnsureDNSAddress(ip string) error {
	// Validate before shelling out. ip is an internal constant today
	// (netconst.DNSAnycastIP), but parsing guards against a future caller
	// passing tainted input into the exec.
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return fmt.Errorf("assign dns address: %q is not a valid IPv4 address", ip)
	}
	cidr := parsed.String() + "/32"
	out, err := exec.Command("ip", "addr", "add", cidr, "dev", "lo").CombinedOutput() //nolint:gosec // cidr is built from a validated net.IP
	if err != nil && !addrAlreadyAssigned(string(out)) {
		return fmt.Errorf("assign dns address %s to lo: %w (output: %s)", ip, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// addrAlreadyAssigned reports whether an `ip addr add` failure just means the
// address was already present (idempotent success). iproute2 wording varies by
// version: older builds print "RTNETLINK answers: File exists", newer ones
// "Error: ipv4: Address already assigned.".
func addrAlreadyAssigned(out string) bool {
	o := strings.ToLower(out)
	return strings.Contains(o, "file exists") || strings.Contains(o, "already assigned")
}

// CreateTAPDevice creates a persistent TAP device by name. It is idempotent: a
// device that already exists is treated as success. Firecracker requires the tap
// to exist before it opens it; unlike QEMU it does not create taps itself.
func CreateTAPDevice(name string) error {
	out, err := exec.Command("ip", "tuntap", "add", "dev", name, "mode", "tap").CombinedOutput()
	if err != nil && !strings.Contains(string(out), "File exists") {
		return fmt.Errorf("create tap %s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DeleteTAPDevice removes a persistent TAP device created by CreateTAPDevice.
func DeleteTAPDevice(name string) error {
	out, err := exec.Command("ip", "tuntap", "del", "dev", name, "mode", "tap").CombinedOutput()
	if err != nil {
		return fmt.Errorf("delete tap %s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DestroyBridge removes a Linux bridge interface.
func DestroyBridge(name string) error {
	if err := ipLink("del", name); err != nil {
		return fmt.Errorf("destroy bridge %s: %w", name, err)
	}
	return nil
}

// AttachTAP adds a TAP interface to a bridge using ip link set master.
func AttachTAP(tapName, bridgeName string) error {
	// A bridge runs at the lowest MTU of its ports: match the bridge first.
	if err := setMTU(tapName); err != nil {
		return err
	}
	if err := ipLink("set", tapName, "master", bridgeName); err != nil {
		return fmt.Errorf("attach %s to bridge %s: %w", tapName, bridgeName, err)
	}
	if err := ipLink("set", tapName, "up"); err != nil {
		return fmt.Errorf("bring up %s: %w", tapName, err)
	}
	return nil
}

// DetachTAP removes a TAP interface from its bridge.
func DetachTAP(tapName string) error {
	if err := ipLink("set", tapName, "nomaster"); err != nil {
		return fmt.Errorf("detach %s from bridge: %w", tapName, err)
	}
	return nil
}

// enableForwarding enables IPv4 forwarding on the host (best-effort).
func enableForwarding() error { //nolint:unparam // error kept for interface consistency
	_ = exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run() //nolint:noctx
	return nil
}

func ipLink(action string, args ...string) error {
	cmdArgs := append([]string{"link", action}, args...)
	out, err := exec.Command("ip", cmdArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip link %s: %w (output: %s)", action, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ipAddr(action string, args ...string) error {
	cmdArgs := append([]string{"addr", action}, args...)
	out, err := exec.Command("ip", cmdArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip addr %s: %w (output: %s)", action, err, strings.TrimSpace(string(out)))
	}
	return nil
}
