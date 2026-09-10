package vm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/AitorConS/jerboa/internal/network"
)

type nativeNetworkState map[string]*nativeNetworkGroup

type nativeNetworkGroup struct {
	network       *network.NativeNetwork
	refs          int
	cidr, gateway string
}

func (m *QEMUManager) prepareHostVM(v *VM) (func(), error) {
	if v.Cfg.EmulateX86 {
		v.AddWarning("x86 guest uses explicit TCG CPU emulation; ARM64 guests use native HVF")
	}
	key := v.Cfg.NetworkName
	ip, gateway, mask := v.Cfg.IPAddress, v.Cfg.GatewayIP, v.Cfg.SubnetMask
	if key == "" {
		key = "vm:" + v.ID
		ip = "10.0.2.15"
		gateway = "10.0.2.2"
		mask = "24"
	}
	if mask == "" {
		mask = "24"
	}
	_, subnet, err := net.ParseCIDR(ip + "/" + mask)
	if err != nil {
		return nil, fmt.Errorf("native network: %w", err)
	}
	if !subnet.Contains(net.ParseIP(gateway)) {
		return nil, fmt.Errorf("gateway %s is outside %s", gateway, subnet)
	}
	g, err := m.acquireNativeNetwork(key, subnet.String(), gateway)
	if err != nil {
		return nil, err
	}
	var ports []network.PortForward
	var link *network.NativeLink
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			if link != nil {
				link.Close()
			}
			for _, p := range ports {
				proto := p.Protocol
				if proto == "" {
					proto = "tcp"
				}
				_ = g.network.Unexpose(proto, net.JoinHostPort(p.BindAddr, fmt.Sprint(p.HostPort)))
			}
			m.nativeMu.Lock()
			defer m.nativeMu.Unlock()
			g.refs--
			if g.refs == 0 {
				g.network.Close()
				delete(m.nativeState, key)
			}
		})
	}
	// Setup owns a reference until success or rollback.
	failed := func(err error) (func(), error) { cleanup(); return nil, err }
	for _, p := range toNetworkPortForwards(v.Cfg.PortMaps) {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		if err := g.network.Expose(proto, net.JoinHostPort(p.BindAddr, fmt.Sprint(p.HostPort)), net.JoinHostPort(ip, fmt.Sprint(p.GuestPort))); err != nil {
			return failed(fmt.Errorf("publish ports: %w", err))
		}
		ports = append(ports, p)
	}
	v.Cfg.nativeSocket = filepath.Join(qmpRuntimeDir(), "net-"+v.ID+".sock")
	sum := sha256.Sum256([]byte(v.ID))
	v.Cfg.nativeMAC = fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
	_ = os.Remove(v.Cfg.nativeSocket)
	link, err = g.network.Listen(v.Cfg.nativeSocket)
	if err != nil {
		return failed(err)
	}
	v.mu.Lock()
	v.hostCleanup = cleanup
	v.healthAddress = ip
	v.healthDial = g.network.DialContext
	v.networkStats = link.Stats
	v.mu.Unlock()
	return cleanup, nil
}

func (m *QEMUManager) acquireNativeNetwork(key, cidr, gateway string) (*nativeNetworkGroup, error) {
	m.nativeMu.Lock()
	defer m.nativeMu.Unlock()
	if m.nativeState == nil {
		m.nativeState = make(nativeNetworkState)
	}
	g := m.nativeState[key]
	if g == nil {
		n, err := network.NewNativeNetwork(cidr, gateway, m.guestDNS)
		if err != nil {
			return nil, err
		}
		g = &nativeNetworkGroup{network: n, cidr: cidr, gateway: gateway}
		m.nativeState[key] = g
	} else if g.cidr != cidr || g.gateway != gateway {
		return nil, fmt.Errorf("inconsistent configuration for network %s", key)
	}
	g.refs++
	return g, nil
}

// QEMU reconnects its private stream after daemon replacement. Rebuild the
// switch, port bindings, probes and collectors for every adopted process.
func (m *QEMUManager) RestoreHostRuntime(ctx context.Context) error {
	for _, v := range m.List() {
		if v.GetState() != StateRunning {
			continue
		}
		if _, err := m.prepareHostVM(v); err != nil {
			return err
		}
		if v.Cfg.CPUShares > 0 || v.Cfg.MemoryMax > 0 {
			if err := m.applyLimits(v, v.pid); err != nil {
				return err
			}
		}
		m.hchecker.Start(ctx, v)
		pid := v.pid
		v.SetStatsProvider(newStatsCollector(pid, v).Collect)
	}
	go func() {
		<-ctx.Done()
		for _, v := range m.List() {
			v.mu.RLock()
			cleanup := v.hostCleanup
			v.mu.RUnlock()
			if cleanup != nil {
				cleanup()
			}
		}
	}()
	return nil
}
