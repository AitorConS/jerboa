//go:build linux

package vm

import (
	"fmt"
	"strconv"
	"strings"
)

// PortProtocol is the transport layer protocol for a port mapping.
type PortProtocol string

const (
	// ProtocolTCP is the TCP protocol.
	ProtocolTCP PortProtocol = "tcp"
	// ProtocolUDP is the UDP protocol.
	ProtocolUDP PortProtocol = "udp"
)

// PortMap describes a single host-to-guest port forwarding rule.
type PortMap struct {
	// HostPort is the port on the host to listen on.
	HostPort uint16
	// GuestPort is the port inside the VM to forward to.
	GuestPort uint16
	// Protocol is "tcp" or "udp"; defaults to "tcp".
	Protocol PortProtocol
}

// String returns the canonical "host:guest/proto" representation.
func (p PortMap) String() string {
	return fmt.Sprintf("%d:%d/%s", p.HostPort, p.GuestPort, p.Protocol)
}

// ParsePortMap parses a port mapping string in one of these forms:
//
//	"8080:80"        → tcp, host=8080, guest=80
//	"8080:80/tcp"    → tcp, host=8080, guest=80
//	"5353:53/udp"    → udp, host=5353, guest=53
func ParsePortMap(s string) (PortMap, error) {
	proto := ProtocolTCP
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		p := PortProtocol(strings.ToLower(s[idx+1:]))
		if p != ProtocolTCP && p != ProtocolUDP {
			return PortMap{}, fmt.Errorf("port map %q: unknown protocol %q (want tcp or udp)", s, p)
		}
		proto = p
		s = s[:idx]
	}

	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return PortMap{}, fmt.Errorf("port map %q: expected host:guest format", s)
	}
	host, err := parsePort(parts[0])
	if err != nil {
		return PortMap{}, fmt.Errorf("port map %q: host port: %w", s, err)
	}
	guest, err := parsePort(parts[1])
	if err != nil {
		return PortMap{}, fmt.Errorf("port map %q: guest port: %w", s, err)
	}
	return PortMap{HostPort: host, GuestPort: guest, Protocol: proto}, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q: %w", s, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("port must be > 0")
	}
	return uint16(n), nil
}

// ParsePortMaps parses multiple port mapping strings (e.g. from CLI -p flags).
func ParsePortMaps(specs []string) ([]PortMap, error) {
	out := make([]PortMap, 0, len(specs))
	for _, s := range specs {
		pm, err := ParsePortMap(s)
		if err != nil {
			return nil, err
		}
		out = append(out, pm)
	}
	return out, nil
}
