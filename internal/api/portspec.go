package api

import (
	"fmt"
	"strconv"
	"strings"
)

// ParsePortMaps parses a list of "host:guest[/tcp|udp]" specs into wire port
// maps. It is a client-side helper so the CLI can build RunParams without
// importing the daemon's vm package.
func ParsePortMaps(specs []string) ([]PortMapSpec, error) {
	out := make([]PortMapSpec, 0, len(specs))
	for _, s := range specs {
		pm, err := ParsePortMap(s)
		if err != nil {
			return nil, err
		}
		out = append(out, pm)
	}
	return out, nil
}

// ParsePortMap parses a single "host:guest[/tcp|udp]" port spec. The protocol
// defaults to tcp.
func ParsePortMap(s string) (PortMapSpec, error) {
	proto := "tcp"
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		p := strings.ToLower(s[idx+1:])
		if p != "tcp" && p != "udp" {
			return PortMapSpec{}, fmt.Errorf("port map %q: unknown protocol %q (want tcp or udp)", s, p)
		}
		proto = p
		s = s[:idx]
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return PortMapSpec{}, fmt.Errorf("port map %q: expected host:guest format", s)
	}
	host, err := parsePortNum(parts[0])
	if err != nil {
		return PortMapSpec{}, fmt.Errorf("port map %q: host port: %w", s, err)
	}
	guest, err := parsePortNum(parts[1])
	if err != nil {
		return PortMapSpec{}, fmt.Errorf("port map %q: guest port: %w", s, err)
	}
	return PortMapSpec{HostPort: host, GuestPort: guest, Protocol: proto}, nil
}

func parsePortNum(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	if n == 0 {
		return 0, fmt.Errorf("port %q must be greater than 0", s)
	}
	return uint16(n), nil
}
