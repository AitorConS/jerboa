package network

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
)

// Published-port modes (jerboad --port-publish).
const (
	// PublishAuto uses kernel NAT where it is available (Linux with nftables
	// and CAP_NET_ADMIN, outside WSL2) and the userspace proxy otherwise.
	PublishAuto = "auto"
	// PublishNAT requires kernel NAT; the daemon fails to start without it.
	PublishNAT = "nat"
	// PublishProxy only uses the userspace proxy (Forwarder), as before.
	PublishProxy = "proxy"
	// PublishNATLAN also translates connections from other hosts, as Docker
	// does. They are forwarded to the guest, so jerboad accepts them in the
	// filter FORWARD chain, and they bypass the host's INPUT rules (ufw).
	// Opt-in; the daemon fails to start without kernel NAT.
	PublishNATLAN = "nat-lan"
)

// natEntry is one published port translated by the kernel: connections made
// on this host to bind:hostPort (any local address when bind is nil) go to
// guest:guestPort.
type natEntry struct {
	bind      net.IP // IPv4, or nil for every local address
	proto     string // "tcp" or "udp"
	hostPort  uint16
	guest     net.IP // IPv4
	guestPort uint16
}

// portNAT installs and removes kernel port translations.
type portNAT interface {
	// prepareBridge lets host-local clients (127.0.0.1) reach guests on bridge.
	prepareBridge(bridge string) error
	add(entries []natEntry) error
	remove(entries []natEntry) error
	shutdown()
}

var (
	natMu     sync.Mutex
	activeNAT portNAT // nil: userspace proxy only
	// clearStaleNAT removes kernel state a previous daemon left when this
	// one runs proxy-only; tests replace it so they never touch the host.
	clearStaleNAT = clearKernelNAT
)

// InitPortPublishing selects how published ports reach guests and returns the
// effective mode (PublishNAT, PublishNATLAN or PublishProxy). With PublishAuto
// it falls back to the proxy when kernel NAT is unavailable. It clears
// translations left by a previous daemon: their listeners died with it, as did
// their proxies.
func InitPortPublishing(mode string) (string, error) {
	switch mode {
	case "", PublishAuto, PublishNAT, PublishNATLAN, PublishProxy:
	default:
		return "", fmt.Errorf("port publishing mode %q is invalid (want %s, %s, %s or %s)",
			mode, PublishAuto, PublishNAT, PublishNATLAN, PublishProxy)
	}
	natMu.Lock()
	defer natMu.Unlock()
	if activeNAT != nil {
		activeNAT.shutdown()
		activeNAT = nil
	}
	if mode == PublishProxy {
		clearStaleNAT()
		return PublishProxy, nil
	}
	lan := mode == PublishNATLAN
	n, err := newKernelNAT(lan)
	if err != nil {
		if mode == PublishNAT || lan {
			return "", fmt.Errorf("port publishing: kernel NAT unavailable: %w", err)
		}
		slog.Info("port publishing: using the userspace proxy", "reason", err)
		return PublishProxy, nil
	}
	activeNAT = n
	if lan {
		return PublishNATLAN, nil
	}
	return PublishNAT, nil
}

// ShutdownPortPublishing removes every kernel translation; published ports
// stop with the daemon, as their userspace listeners do.
func ShutdownPortPublishing() {
	natMu.Lock()
	defer natMu.Unlock()
	if activeNAT != nil {
		activeNAT.shutdown()
		activeNAT = nil
	}
}

func currentNAT() portNAT {
	natMu.Lock()
	defer natMu.Unlock()
	return activeNAT
}

// natEntries maps port forwards to kernel translations. Ports bound to an
// IPv6 address stay on the proxy only.
func natEntries(guestIP string, ports []PortForward) ([]natEntry, error) {
	guest := net.ParseIP(guestIP).To4()
	if guest == nil {
		return nil, fmt.Errorf("guest IP %q is not IPv4", guestIP)
	}
	var out []natEntry
	for _, pm := range ports {
		proto := "tcp"
		if pm.Protocol == "udp" || pm.Protocol == "UDP" {
			proto = "udp"
		}
		var bind net.IP
		switch pm.BindAddr {
		case "", "0.0.0.0", "::":
		default:
			bind = net.ParseIP(pm.BindAddr).To4()
			if bind == nil {
				continue
			}
		}
		out = append(out, natEntry{bind: bind, proto: proto, hostPort: pm.HostPort, guest: guest, guestPort: pm.GuestPort})
	}
	return out, nil
}
