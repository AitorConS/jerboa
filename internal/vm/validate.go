//go:build linux || (darwin && arm64)

package vm

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// networkNameRe matches valid Linux network interface names: 1–15 characters
// from a conservative set. IFNAMSIZ caps interface names at 15 bytes.
var logicalNetworkNameRe = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

var networkNameRe = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`)

// memoryRe matches QEMU memory strings: a positive integer with an optional
// binary unit suffix (e.g. "512", "256M", "1G", "2GiB").
var memoryRe = regexp.MustCompile(`^[1-9][0-9]*([KkMmGgTt]i?[Bb]?)?$`)

// envKeyRe matches a valid environment variable name.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// hasBootArgUnsafe reports whether s contains whitespace or control characters
// that would break or inject into the space-separated kernel boot-args line the
// Firecracker backend builds (env and mounts are delivered as boot args, not
// fw_cfg). QEMU uses fw_cfg, but the same values feed both backends, so the
// constraint is applied uniformly.
func hasBootArgUnsafe(s string) bool {
	return strings.ContainsAny(s, " \t\r\n\v\f\x00")
}

// validateVMConfig checks user-supplied VM configuration before it is turned
// into a QEMU command line. It validates the syntactic shape of network, memory
// and address fields so malformed input fails fast with a descriptive error
// naming the offending field, instead of producing a cryptic QEMU error.
//
// It deliberately does not touch the filesystem: image and volume paths are
// resolved and existence-checked by the image store layer before reaching here.
func validateVMConfig(cfg Config) error {
	if cfg.ImagePath == "" {
		return fmt.Errorf("validate config: ImagePath is required")
	}

	if cfg.Memory == "" {
		return fmt.Errorf("validate config: Memory is required (e.g. 256M, 1G)")
	}
	if !memoryRe.MatchString(cfg.Memory) {
		return fmt.Errorf("validate config: Memory %q is invalid (want a size like 256M or 1G)", cfg.Memory)
	}

	if cfg.CPUs < 0 {
		return fmt.Errorf("validate config: CPUs must be >= 0, got %d", cfg.CPUs)
	}

	if cfg.NetworkName != "" && !logicalNetworkNameRe.MatchString(cfg.NetworkName) {
		return fmt.Errorf("validate config: NetworkName %q is invalid (1-128 chars, [a-zA-Z0-9_.:-])", cfg.NetworkName)
	}
	if cfg.BridgeName != "" && !networkNameRe.MatchString(cfg.BridgeName) {
		return fmt.Errorf("validate config: BridgeName %q is invalid (1-15 chars, [a-zA-Z0-9_.:-])", cfg.BridgeName)
	}

	if hc := cfg.HealthCheck; hc != nil {
		if (hc.Type != "tcp" && hc.Type != "http") || hc.Port < 1 || hc.Port > 65535 {
			return fmt.Errorf("invalid health check: expected tcp/http and port 1-65535")
		}
	}
	for i, pm := range cfg.PortMaps {
		if pm.Protocol != "" && pm.Protocol != ProtocolTCP && pm.Protocol != ProtocolUDP {
			return fmt.Errorf("validate config: PortMaps[%d] protocol %q invalid (want tcp or udp)", i, pm.Protocol)
		}
		if pm.HostPort == 0 || pm.GuestPort == 0 {
			return fmt.Errorf("validate config: PortMaps[%d] host and guest ports must be 1-65535", i)
		}
	}

	if cfg.IPAddress != "" && net.ParseIP(cfg.IPAddress) == nil {
		return fmt.Errorf("validate config: IPAddress %q is not a valid IP", cfg.IPAddress)
	}
	if cfg.GatewayIP != "" && net.ParseIP(cfg.GatewayIP) == nil {
		return fmt.Errorf("validate config: GatewayIP %q is not a valid IP", cfg.GatewayIP)
	}
	if cfg.SubnetMask != "" {
		mask, err := strconv.Atoi(cfg.SubnetMask)
		if err != nil || mask < 0 || mask > 32 {
			return fmt.Errorf("validate config: SubnetMask %q is invalid (want 0-32)", cfg.SubnetMask)
		}
	}

	for i, kv := range cfg.Env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("validate config: Env[%d] %q must be KEY=VALUE", i, kv)
		}
		if !envKeyRe.MatchString(key) {
			return fmt.Errorf("validate config: Env[%d] key %q is invalid (want [A-Za-z_][A-Za-z0-9_]*)", i, key)
		}
		if hasBootArgUnsafe(val) {
			return fmt.Errorf("validate config: Env[%d] value for %q contains whitespace or control characters, "+
				"which cannot be delivered safely via kernel boot args", i, key)
		}
	}

	for i, vol := range cfg.Volumes {
		if vol.DiskPath == "" {
			return fmt.Errorf("validate config: Volumes[%d] DiskPath is required", i)
		}
		if hasBootArgUnsafe(vol.Label) || strings.ContainsAny(vol.Label, "=:.") {
			return fmt.Errorf("validate config: Volumes[%d] Label %q must not contain whitespace, control characters, '=', ':', or '.'", i, vol.Label)
		}
		if vol.GuestPath != "" && (!strings.HasPrefix(vol.GuestPath, "/") || hasBootArgUnsafe(vol.GuestPath)) {
			return fmt.Errorf("validate config: Volumes[%d] GuestPath %q must be an absolute path without whitespace or control characters", i, vol.GuestPath)
		}
	}

	return nil
}
