//go:build linux

package network

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreate(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	n, err := s.Create("testnet", "", "bridge")
	require.NoError(t, err)
	require.Equal(t, "testnet", n.Name)
	require.Equal(t, "bridge", n.Driver)
	require.Contains(t, n.Bridge, "jerboa-br-")
	require.Contains(t, n.Subnet, "/24")
	require.NotEmpty(t, n.Gateway)
}

func TestCreateDuplicate(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.Create("testnet", "", "bridge")
	require.NoError(t, err)

	_, err = s.Create("testnet", "", "bridge")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

func TestCreateEmptyName(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.Create("", "", "bridge")
	require.Error(t, err)
}

func TestCreateWithSubnet(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	n, err := s.Create("my-net", "172.20.0.0/24", "bridge")
	require.NoError(t, err)
	require.Equal(t, "172.20.0.0/24", n.Subnet)
	require.Equal(t, "172.20.0.1", n.Gateway)
	require.Equal(t, "jerboa-br-my-net", n.Bridge)
}

func TestCreateDefaultDriver(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	n, err := s.Create("testnet", "10.0.0.0/24", "")
	require.NoError(t, err)
	require.Equal(t, "bridge", n.Driver)
}

func TestCreateInvalidSubnet(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.Create("bad", "not-a-cidr", "bridge")
	require.Error(t, err)
}

func TestGet(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	created, err := s.Create("testnet", "10.0.0.0/24", "bridge")
	require.NoError(t, err)

	got, err := s.Get("testnet")
	require.NoError(t, err)
	require.Equal(t, created.Name, got.Name)
	require.Equal(t, created.Subnet, got.Subnet)
	require.Equal(t, created.Gateway, got.Gateway)
}

func TestGetNotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.Get("nonexistent")
	require.Error(t, err)
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.Create("net1", "10.0.1.0/24", "bridge")
	require.NoError(t, err)
	_, err = s.Create("net2", "10.0.2.0/24", "bridge")
	require.NoError(t, err)

	nets, err := s.List()
	require.NoError(t, err)
	require.Len(t, nets, 2)
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.Create("testnet", "10.0.0.0/24", "bridge")
	require.NoError(t, err)

	err = s.Remove("testnet")
	require.NoError(t, err)

	_, err = s.Get("testnet")
	require.Error(t, err)
}

func TestRemoveNotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	err = s.Remove("nonexistent")
	require.Error(t, err)
}

func TestAllocateIP(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.Create("testnet", "10.0.0.0/24", "bridge")
	require.NoError(t, err)

	ip, err := s.AllocateIP("testnet")
	require.NoError(t, err)
	require.NotNil(t, ip)
	require.Equal(t, "10.0.0.2", ip.String())

	ip2, err := s.AllocateIP("testnet")
	require.NoError(t, err)
	require.Equal(t, "10.0.0.3", ip2.String())
}

func TestAllocateIPSequential(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.Create("testnet", "10.50.0.0/24", "bridge")
	require.NoError(t, err)

	for i := 2; i <= 5; i++ {
		ip, err := s.AllocateIP("testnet")
		require.NoError(t, err)
		expected := net.ParseIP("10.50.0.2").To4()
		expected[3] = byte(i)
		require.Equal(t, expected.String(), ip.String())
	}
}

func TestAllocateIPNetworkNotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.AllocateIP("nonexistent")
	require.Error(t, err)
}

func TestReleaseIP(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.Create("testnet", "10.0.0.0/24", "bridge")
	require.NoError(t, err)

	ip, err := s.AllocateIP("testnet")
	require.NoError(t, err)

	err = s.ReleaseIP("testnet", ip.String())
	require.NoError(t, err)
}

func TestReleaseIPNotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	_, err = s.Create("testnet", "10.0.0.0/24", "bridge")
	require.NoError(t, err)

	err = s.ReleaseIP("testnet", "10.0.0.99")
	require.NoError(t, err)
}

func TestReleaseIPNetworkNotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	err = s.ReleaseIP("nonexistent", "10.0.0.2")
	require.Error(t, err)
}

func TestAutoSubnetAllocation(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	n1, err := s.Create("net1", "", "bridge")
	require.NoError(t, err)
	require.Contains(t, n1.Subnet, "/24")

	n2, err := s.Create("net2", "", "bridge")
	require.NoError(t, err)
	require.NotEqual(t, n1.Subnet, n2.Subnet)
}

func TestParseSubnet(t *testing.T) {
	ipNet, gw, err := parseSubnet("192.168.1.0/24")
	require.NoError(t, err)
	require.Equal(t, "192.168.1.0/24", ipNet.String())
	require.Equal(t, "192.168.1.1", gw.String())
}

func TestParseSubnetInvalid(t *testing.T) {
	_, _, err := parseSubnet("not-a-cidr")
	require.Error(t, err)
}

func TestNewStoreCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "networks")
	_, err := NewStore(dir)
	require.NoError(t, err)
	_, err = os.Stat(dir)
	require.NoError(t, err)
}

func TestNextIP(t *testing.T) {
	_, ipNet, _ := net.ParseCIDR("10.0.0.0/24")

	ip := nextIP(ipNet, 2)
	require.Equal(t, "10.0.0.2", ip.String())

	ip = nextIP(ipNet, 10)
	require.Equal(t, "10.0.0.10", ip.String())

	ip = nextIP(ipNet, 254)
	require.Equal(t, "10.0.0.254", ip.String())
}

func TestAllocateSubnet(t *testing.T) {
	_, baseNet, _ := net.ParseCIDR("10.100.0.0/16")

	allocated := map[string]bool{}
	ipNet, gw, err := allocateSubnet(baseNet, allocated)
	require.NoError(t, err)
	require.Equal(t, "10.100.0.0/24", ipNet.String())
	require.Equal(t, "10.100.0.1", gw.String())

	allocated[ipNet.String()] = true
	ipNet2, gw2, err := allocateSubnet(baseNet, allocated)
	require.NoError(t, err)
	require.Equal(t, "10.100.1.0/24", ipNet2.String())
	require.Equal(t, "10.100.1.1", gw2.String())
}
