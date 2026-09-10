//go:build linux || (darwin && arm64)

package network

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AitorConS/jerboa/internal/naming"
)

// ErrIPAlreadyAllocated is returned by ReserveIP when the requested address is
// already assigned within the network. It is a sentinel so callers can tell a
// genuine conflict from an out-of-range or unknown-network error.
var ErrIPAlreadyAllocated = errors.New("ip already allocated")

const (
	metaFile          = "meta.json"
	stateFile         = "state.json"
	defaultSubnetCIDR = "10.100.0.0/16"
	bridgePrefix      = "jerboa-br-"
	tapPrefix         = "jerboa-tap-"
	// maxIfaceNameLen is the Linux interface name limit (IFNAMSIZ - 1). A bridge
	// name derived from a network name must fit within it.
	maxIfaceNameLen = 15
)

// deriveBridgeName returns a Linux bridge name for a network that always fits
// within maxIfaceNameLen. Short names are used verbatim (readable, e.g.
// "jerboa-br-app"); longer names fall back to a short hash so that a valid
// network name can never produce an over-long, unusable bridge name.
func deriveBridgeName(name string) string {
	candidate := bridgePrefix + name
	if len(candidate) <= maxIfaceNameLen {
		return candidate
	}
	sum := sha256.Sum256([]byte(name))
	return bridgePrefix + hex.EncodeToString(sum[:])[:maxIfaceNameLen-len(bridgePrefix)]
}

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
	if err := naming.ValidateResourceName("network", name); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.networkDir(name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("network %q already exists", name)
	}

	if driver == "" {
		driver = "bridge"
	}

	// Existing subnets are needed by both branches: the auto-allocator must skip
	// them, and an explicit subnet must be rejected if it overlaps one. Two
	// networks sharing (or overlapping) a subnet produce duplicate kernel routes
	// to the same range on different bridges, so traffic is routed ambiguously and
	// published ports / inter-VM connectivity silently break.
	allocated, err := s.allocatedSubnetsLocked()
	if err != nil {
		return nil, fmt.Errorf("read existing subnets: %w", err)
	}

	var ipNet *net.IPNet
	var gatewayIP net.IP

	if subnet == "" {
		_, defIPNet, _ := net.ParseCIDR(defaultSubnetCIDR)
		ipNet, gatewayIP, err = allocateSubnet(defIPNet, allocated)
		if err != nil {
			return nil, fmt.Errorf("allocate subnet: %w", err)
		}
	} else {
		ipNet, gatewayIP, err = parseSubnet(subnet)
		if err != nil {
			return nil, fmt.Errorf("invalid subnet %q: %w", subnet, err)
		}
		if conflict := overlappingSubnet(ipNet, allocated); conflict != "" {
			return nil, fmt.Errorf("subnet %s overlaps existing network %q", ipNet.String(), conflict)
		}
	}

	n := &Network{
		ID:        name,
		Name:      name,
		Subnet:    ipNet.String(),
		Gateway:   gatewayIP.String(),
		Bridge:    deriveBridgeName(name),
		Driver:    driver,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.ensureBridgeAvailableLocked(name, n.Bridge); err != nil {
		return nil, err
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

	dir, err := s.networkDir(name)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		return fmt.Errorf("network %q not found", name)
	}
	// Capture the bridge before deleting the record so the Linux bridge (and the
	// kernel route bound to it) is torn down too. Removing only the on-disk record
	// leaked the bridge: its route kept competing with any later network that
	// reused the subnet, silently breaking that network's connectivity until the
	// orphan bridge was deleted by hand.
	bridge := ""
	if n, readErr := s.readMeta(name); readErr == nil {
		bridge = n.Bridge
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("network remove %s: %w", name, err)
	}
	if bridge != "" {
		if err := DestroyBridge(bridge); err != nil {
			// Best-effort: the bridge may never have been created (no VM ever
			// joined this network) or may already be gone. Neither is fatal to
			// removing the logical network.
			slog.Debug("network remove: destroy bridge", "network", name, "bridge", bridge, "err", err)
		}
	}
	return nil
}

// ReserveIP records ip as allocated within the named network, failing if it is
// already assigned (ErrIPAlreadyAllocated) or falls outside the subnet. It backs
// static-IP validation: a user-supplied --ip must be reserved so a second VM
// cannot be given the same address (which collides on the bridge and breaks
// both VMs' connectivity), and so the dynamic allocator skips it.
func (s *Store) ReserveIP(name, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, err := s.readMeta(name)
	if err != nil {
		return fmt.Errorf("reserve ip: %w", err)
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return fmt.Errorf("reserve ip: %q is not a valid IPv4 address", ip)
	}
	_, ipNet, err := net.ParseCIDR(n.Subnet)
	if err != nil {
		return fmt.Errorf("reserve ip parse subnet: %w", err)
	}
	if !ipNet.Contains(parsed) {
		return fmt.Errorf("reserve ip: %s is outside network %q subnet %s", ip, name, n.Subnet)
	}
	// The subnet (network) address and the directed broadcast address are not
	// assignable to a host: a guest configured with either cannot communicate.
	// Reject them so a bad --ip fails fast instead of booting an unreachable VM.
	if networkOrBroadcast(ipNet, parsed) {
		return fmt.Errorf("reserve ip: %s is the network or broadcast address of %q subnet %s", ip, name, n.Subnet)
	}

	st, err := s.readState(name)
	if err != nil {
		return fmt.Errorf("reserve ip read state: %w", err)
	}
	want := parsed.String()
	for _, aip := range st.AllocatedIPs {
		if aip == want {
			return fmt.Errorf("%w: %s in network %q", ErrIPAlreadyAllocated, want, name)
		}
	}
	st.AllocatedIPs = append(st.AllocatedIPs, want)

	dir, err := s.networkDir(name)
	if err != nil {
		return err
	}
	if err := writeNetworkState(dir, st); err != nil {
		return fmt.Errorf("reserve ip write state: %w", err)
	}
	return nil
}

// networkOrBroadcast reports whether ip is the IPv4 subnet (network) address or
// the directed broadcast address of ipNet — neither is a usable host address.
func networkOrBroadcast(ipNet *net.IPNet, ip net.IP) bool {
	base := ipNet.IP.To4()
	v4 := ip.To4()
	if base == nil || v4 == nil || len(ipNet.Mask) != net.IPv4len {
		return false
	}
	if v4.Equal(base) {
		return true // network address
	}
	bcast := make(net.IP, net.IPv4len)
	for i := 0; i < net.IPv4len; i++ {
		bcast[i] = base[i] | ^ipNet.Mask[i]
	}
	return v4.Equal(bcast)
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

	// Skip any address already assigned — the gateway, previously allocated IPs,
	// and reserved static IPs (see ReserveIP). A plain monotonic counter would
	// otherwise re-hand-out a static address that happens to fall inside the
	// dynamic range, producing the very duplicate-IP collision static reservation
	// is meant to prevent.
	allocated := make(map[string]bool, len(st.AllocatedIPs))
	for _, a := range st.AllocatedIPs {
		allocated[a] = true
	}
	var ip net.IP
	for {
		candidate := nextIP(ipNet, st.NextIndex)
		if candidate == nil {
			return nil, fmt.Errorf("network %q: no available IPs in subnet %s", name, n.Subnet)
		}
		st.NextIndex++
		if !allocated[candidate.String()] {
			ip = candidate
			break
		}
	}

	st.AllocatedIPs = append(st.AllocatedIPs, ip.String())

	dir, err := s.networkDir(name)
	if err != nil {
		return nil, err
	}
	if err := writeNetworkState(dir, st); err != nil {
		return nil, fmt.Errorf("allocate ip write state: %w", err)
	}

	return ip, nil
}

func (s *Store) ReleaseIP(name string, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.networkDir(name)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
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

func (s *Store) networkDir(name string) (string, error) {
	if err := naming.ValidateResourceName("network", name); err != nil {
		return "", err
	}
	return naming.SafeJoin(s.root, name)
}

func (s *Store) readMeta(name string) (*Network, error) {
	dir, err := s.networkDir(name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, metaFile)
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
	dir, err := s.networkDir(name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, stateFile)
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

// allocatedNet pairs a parsed subnet with the network that owns it, so overlap
// errors can name the conflicting network.
type allocatedNet struct {
	ipNet *net.IPNet
	name  string
}

func (s *Store) allocatedSubnetsLocked() ([]allocatedNet, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("read network dirs: %w", err)
	}
	var result []allocatedNet
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := s.readMeta(e.Name())
		if err != nil {
			continue
		}
		_, ipNet, err := net.ParseCIDR(n.Subnet)
		if err != nil {
			continue
		}
		result = append(result, allocatedNet{ipNet: ipNet, name: n.Name})
	}
	return result, nil
}

// subnetsOverlap reports whether two IPv4 networks share any address. Either
// network containing the other's base address is sufficient: for CIDR ranges
// that implies the smaller is wholly inside the larger.
func subnetsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// overlappingSubnet returns the name of the first allocated network whose subnet
// overlaps candidate, or "" when none does.
func overlappingSubnet(candidate *net.IPNet, allocated []allocatedNet) string {
	for _, a := range allocated {
		if subnetsOverlap(candidate, a.ipNet) {
			return a.name
		}
	}
	return ""
}

func (s *Store) ensureBridgeAvailableLocked(name, bridge string) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("read network dirs: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := s.readMeta(e.Name())
		if err != nil {
			continue
		}
		if n.Name != name && n.Bridge == bridge {
			return fmt.Errorf("network %q bridge name %q collides with existing network %q", name, bridge, n.Name)
		}
	}
	return nil
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

func allocateSubnet(baseNet *net.IPNet, allocated []allocatedNet) (*net.IPNet, net.IP, error) {
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
		// Skip any candidate that overlaps an existing network, not just an exact
		// string match: an operator-created network with a wider or offset mask
		// must not be silently re-used under a different /24.
		if overlappingSubnet(cidr, allocated) == "" {
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
