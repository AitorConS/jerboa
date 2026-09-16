//go:build darwin && arm64

package vm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
)

type fcHostPolicy struct {
	Version   int                `json:"version"`
	Mode      string             `json:"mode,omitempty"`
	Egress    []nativeFCListener `json:"egress,omitempty"`
	Listeners []nativeFCListener `json:"listeners,omitempty"`
	DNS       *struct {
		Address string `json:"address"`
		Port    uint16 `json:"port"`
	} `json:"dns,omitempty"`
}

func (m *FirecrackerManager) sharedPolicy() (fcHostPolicy, error) {
	p := fcHostPolicy{Version: 1}
	if len(m.fcSecurity) > 0 {
		d := json.NewDecoder(bytes.NewReader(m.fcSecurity))
		d.DisallowUnknownFields()
		if err := d.Decode(&p); err != nil {
			return p, err
		}
	}
	if p.Version != 1 || (p.Mode != "" && p.Mode != "hardened") {
		return p, fmt.Errorf("shared networking requires security version 1 hardened mode")
	}
	if len(p.Egress) > 64 || len(p.Listeners) > 0 {
		return p, fmt.Errorf("shared networking permits at most 64 egress rules; listeners are declared by port mappings")
	}
	for _, e := range p.Egress {
		if (e.Protocol != "tcp" && e.Protocol != "udp") || !concreteIPv4(e.Address) || e.Port == 0 {
			return p, fmt.Errorf("invalid shared network egress endpoint")
		}
	}
	if p.DNS != nil && (!concreteIPv4(p.DNS.Address) || p.DNS.Port == 0) {
		return p, fmt.Errorf("invalid shared network DNS endpoint")
	}
	return p, nil
}
func concreteIPv4(value string) bool {
	ip := net.ParseIP(value).To4()
	return ip != nil && ip[0] != 0 && ip[0] < 224
}

func (m *FirecrackerManager) prepareFCHost(v *VM) (func(), error) {
	if v.Cfg.NetworkName == "" {
		// Without a Jerboa network the VMM's own slirp carries the traffic, so no
		// Jerboa link sees the frames: count bytes from the VMM's metrics instead.
		socket := m.vmSockPath(v.ID)
		v.mu.Lock()
		v.networkStats = func() (int64, int64) { return readNativeFCNetStats(socket) }
		v.mu.Unlock()
		return nil, nil
	}
	p, err := m.sharedPolicy()
	if err != nil {
		return nil, err
	}
	// Set once under the same mutex that protects group creation. The policy is
	// manager-wide and immutable while VMs run.
	m.nativeMu.Lock()
	m.nativeEgress = func(_, address string, port uint16, udp bool) bool {
		protocol := "tcp"
		if udp {
			protocol = "udp"
		}
		for _, e := range p.Egress {
			if e.Protocol == protocol && e.Address == address && e.Port == port {
				return true
			}
		}
		return false
	}
	m.nativeMu.Unlock()
	return m.nativeNetworkHost.prepareHostVM(v)
}

func WithFCGuestDNS(fn func([]byte, string) ([]byte, error)) FCOption {
	return func(m *FirecrackerManager) { m.guestDNS = fn }
}

// External DNS is opt-in; service DNS remains local to the selected network.
func (m *FirecrackerManager) GuestDNSUpstream() (string, error) {
	p := fcHostPolicy{Version: 1}
	if len(m.fcSecurity) > 0 {
		if err := json.Unmarshal(m.fcSecurity, &p); err != nil {
			return "", err
		}
	}
	if p.DNS == nil {
		return "", nil
	}
	return net.JoinHostPort(p.DNS.Address, strconv.Itoa(int(p.DNS.Port))), nil
}
