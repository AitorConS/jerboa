//go:build darwin && arm64

package vm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
)

type fcResolver struct {
	Address string `json:"address"`
	Port    uint16 `json:"port"`
}

type fcHostPolicy struct {
	Version   int                `json:"version"`
	Mode      string             `json:"mode,omitempty"`
	Egress    []nativeFCListener `json:"egress,omitempty"`
	Listeners []nativeFCListener `json:"listeners,omitempty"`
	DNS       *fcResolver        `json:"dns,omitempty"`
	// PublicEgress grants TCP and UDP to every public IPv4 address beside the
	// exact Egress rules: the Internet, but not the host's loopback nor
	// private, shared or link-local networks.
	PublicEgress bool `json:"public_egress,omitempty"`
}

// defaultFCPolicy is what guests may reach when the daemon was given no
// security object: public addresses, as QEMU guests do, through the daemon's
// resolver. The host and the networks around it stay out of reach; a
// supplied object replaces this entirely, {"version": 1} closing everything.
func (m *FirecrackerManager) defaultFCPolicy() fcHostPolicy {
	p := fcHostPolicy{Version: 1, PublicEgress: true}
	host, port, err := net.SplitHostPort(m.fcDefaultDNS)
	if n, perr := strconv.ParseUint(port, 10, 16); err == nil && perr == nil && n != 0 && concreteIPv4(host) {
		p.DNS = &fcResolver{Address: net.ParseIP(host).To4().String(), Port: uint16(n)}
	}
	return p
}

// permits reports whether a guest may open a connection to address:port.
func (p fcHostPolicy) permits(address string, port uint16, udp bool) bool {
	protocol := "tcp"
	if udp {
		protocol = "udp"
	}
	for _, e := range p.Egress {
		if e.Protocol == protocol && e.Address == address && e.Port == port {
			return true
		}
	}
	return p.PublicEgress && port != 0 && publicEgressAddress(address)
}

func (m *FirecrackerManager) sharedPolicy() (fcHostPolicy, error) {
	p := m.defaultFCPolicy()
	if len(m.fcSecurity) > 0 {
		p = fcHostPolicy{Version: 1}
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
	m.nativeEgress = func(_, address string, port uint16, udp bool) bool { return p.permits(address, port, udp) }
	m.nativeMu.Unlock()
	return m.nativeNetworkHost.prepareHostVM(v)
}

func WithFCGuestDNS(fn func([]byte, string) ([]byte, error)) FCOption {
	return func(m *FirecrackerManager) { m.guestDNS = fn }
}

// GuestDNSUpstream is the external resolver of guests on a network: the
// supplied security object's, or the daemon's default without one. Service
// DNS remains local to the selected network.
func (m *FirecrackerManager) GuestDNSUpstream() (string, error) {
	p := m.defaultFCPolicy()
	if len(m.fcSecurity) > 0 {
		p = fcHostPolicy{Version: 1}
		if err := json.Unmarshal(m.fcSecurity, &p); err != nil {
			return "", err
		}
	}
	if p.DNS == nil {
		return "", nil
	}
	return net.JoinHostPort(p.DNS.Address, strconv.Itoa(int(p.DNS.Port))), nil
}
