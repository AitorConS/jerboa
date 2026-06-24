//go:build linux

package network

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	metaFile          = "meta.json"
	stateFile         = "state.json"
	defaultSubnetCIDR = "10.100.0.0/16"
	bridgePrefix      = "jerboa-br-"
	tapPrefix         = "jerboa-tap-"
)

type Network struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Subnet    string    `json:"subnet"`
	Gateway   string    `json:"gateway"`
	Bridge    string    `json:"bridge"`
	Driver    string    `json:"driver"`
	CreatedAt time.Time `json:"created_at"`
}

type networkState struct {
	AllocatedIPs []string `json:"allocated_ips"`
	NextIndex    int      `json:"next_index"`
}

type Store struct {
	root string
	mu   sync.RWMutex
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("network store mkdir %s: %w", dir, err)
	}
	return &Store{root: dir}, nil
}

func (s *Store) Create(name, subnet, driver string) (*Network, error) {
	if name == "" {
		return nil, fmt.Errorf("network name must not be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.networkDir(name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("network %q already exists", name)
	}

	if driver == "" {
		driver = "bridge"
	}

	var ipNet *net.IPNet
	var gatewayIP net.IP

	if subnet == "" {
		allocated, err := s.allocatedSubnetsLocked()
		if err != nil {
			return nil, fmt.Errorf("find available subnet: %w", err)
		}
		_, defIPNet, _ := net.ParseCIDR(defaultSubnetCIDR)
		ipNet, gatewayIP, err = allocateSubnet(defIPNet, allocated)
		if err != nil {
			return nil, fmt.Errorf("allocate subnet: %w", err)
		}
	} else {
		var err error
		ipNet, gatewayIP, err = parseSubnet(subnet)
		if err != nil {
			return nil, fmt.Errorf("invalid subnet %q: %w", subnet, err)
		}
	}

	n := &Network{
		ID:        name,
		Name:      name,
		Subnet:    ipNet.String(),
		Gateway:   gatewayIP.String(),
		Bridge:    bridgePrefix + name,
		Driver:    driver,
		CreatedAt: time.Now().UTC(),
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("network create dir %s: %w", dir, err)
	}
	if err := writeNetworkMeta(dir, n); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("network write meta: %w", err)
	}

	st := &networkState{NextIndex: 2}
	if gatewayIP != nil {
		st.AllocatedIPs = append(st.AllocatedIPs, gatewayIP.String())
	}
	if err := writeNetworkState(dir, st); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("network write state: %w", err)
	}

	return n, nil
}

func (s *Store) Get(name string) (*Network, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readMeta(name)
}

func (s *Store) List() ([]*Network, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("network list: %w", err)
	}
	out := make([]*Network, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := s.readMeta(e.Name())
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.networkDir(name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("network %q not found", name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("network remove %s: %w", name, err)
	}
	return nil
}

func (s *Store) AllocateIP(name string) (net.IP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, err := s.readMeta(name)
	if err != nil {
		return nil, fmt.Errorf("allocate ip: %w", err)
	}

	st, err := s.readState(name)
	if err != nil {
		return nil, fmt.Errorf("allocate ip read state: %w", err)
	}

	_, ipNet, err := net.ParseCIDR(n.Subnet)
	if err != nil {
		return nil, fmt.Errorf("allocate ip parse subnet: %w", err)
	}

	ip := nextIP(ipNet, st.NextIndex)
	if ip == nil {
		return nil, fmt.Errorf("network %q: no available IPs in subnet %s", name, n.Subnet)
	}

	st.AllocatedIPs = append(st.AllocatedIPs, ip.String())
	st.NextIndex++

	if err := writeNetworkState(s.networkDir(name), st); err != nil {
		return nil, fmt.Errorf("allocate ip write state: %w", err)
	}

	return ip, nil
}

func (s *Store) ReleaseIP(name string, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.networkDir(name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("network %q not found", name)
	}

	st, err := s.readState(name)
	if err != nil {
		return fmt.Errorf("release ip read state: %w", err)
	}

	found := false
	for i, aip := range st.AllocatedIPs {
		if aip == ip {
			st.AllocatedIPs = append(st.AllocatedIPs[:i], st.AllocatedIPs[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	if err := writeNetworkState(dir, st); err != nil {
		return fmt.Errorf("release ip write state: %w", err)
	}
	return nil
}

func (s *Store) networkDir(name string) string {
	return filepath.Join(s.root, name)
}

func (s *Store) readMeta(name string) (*Network, error) {
	path := filepath.Join(s.networkDir(name), metaFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("network %q not found: %w", name, err)
	}
	var n Network
	if err := json.Unmarshal(data, &n); err != nil {
		return nil, fmt.Errorf("network %q corrupt meta: %w", name, err)
	}
	return &n, nil
}

func (s *Store) readState(name string) (*networkState, error) {
	path := filepath.Join(s.networkDir(name), stateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("network %q read state: %w", name, err)
	}
	var st networkState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("network %q corrupt state: %w", name, err)
	}
	return &st, nil
}

func (s *Store) allocatedSubnetsLocked() (map[string]bool, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("read network dirs: %w", err)
	}
	result := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := s.readMeta(e.Name())
		if err != nil {
			continue
		}
		result[n.Subnet] = true
	}
	return result, nil
}

func writeNetworkMeta(dir string, n *Network) error {
	data, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal network meta: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, metaFile), data, 0o600); err != nil {
		return fmt.Errorf("write network meta: %w", err)
	}
	return nil
}

func writeNetworkState(dir string, st *networkState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal network state: %w", err)
	}
	tmp := filepath.Join(dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write network state tmp: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, stateFile)); err != nil {
		return fmt.Errorf("rename network state: %w", err)
	}
	return nil
}

func parseSubnet(cidr string) (*net.IPNet, net.IP, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, nil, fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	if ipNet.IP.To4() == nil {
		return nil, nil, fmt.Errorf("only IPv4 subnets are supported")
	}
	gw := make(net.IP, len(ipNet.IP))
	copy(gw, ipNet.IP)
	gw[3] = 1
	return ipNet, gw, nil
}

func allocateSubnet(baseNet *net.IPNet, allocated map[string]bool) (*net.IPNet, net.IP, error) {
	baseIP := baseNet.IP.To4()
	if baseIP == nil {
		return nil, nil, fmt.Errorf("base subnet must be IPv4")
	}
	mask := net.CIDRMask(24, 32)
	for i := 0; i < 256; i++ {
		subnetIP := make(net.IP, 4)
		copy(subnetIP, baseIP)
		subnetIP[2] = byte(i)
		subnetIP[3] = 0
		cidr := &net.IPNet{IP: subnetIP, Mask: mask}
		if !allocated[cidr.String()] {
			gw := make(net.IP, 4)
			copy(gw, subnetIP)
			gw[3] = 1
			return cidr, gw, nil
		}
	}
	return nil, nil, fmt.Errorf("no available /24 subnets in %s", baseNet.String())
}

func nextIP(ipNet *net.IPNet, index int) net.IP {
	ip := make(net.IP, len(ipNet.IP))
	copy(ip, ipNet.IP)
	for i := 3; i >= 0; i-- {
		ip[i] += byte(index & 0xFF)
		index >>= 8
	}
	if !ipNet.Contains(ip) {
		return nil
	}
	return ip
}
