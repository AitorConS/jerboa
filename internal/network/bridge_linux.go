//go:build linux

package network

import (
	"fmt"
	"os/exec"
	"strings"
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

// DestroyBridge removes a Linux bridge interface.
func DestroyBridge(name string) error {
	if err := ipLink("del", name); err != nil {
		return fmt.Errorf("destroy bridge %s: %w", name, err)
	}
	return nil
}

// AttachTAP adds a TAP interface to a bridge using ip link set master.
func AttachTAP(tapName, bridgeName string) error {
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
