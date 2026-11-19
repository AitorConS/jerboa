//go:build linux || (darwin && arm64)

package network

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMain keeps the store tests independent of the machine's own networks;
// the tests below swap hostConflict to exercise it.
func TestMain(m *testing.M) {
	hostConflict = func(*net.IPNet) string { return "" }
	os.Exit(m.Run())
}

func withHostRoute(t *testing.T, cidr string) {
	t.Helper()
	_, route, err := net.ParseCIDR(cidr)
	require.NoError(t, err)
	prev := hostConflict
	hostConflict = func(n *net.IPNet) string {
		if subnetsOverlap(n, route) {
			return "route " + cidr + " on eth0"
		}
		return ""
	}
	t.Cleanup(func() { hostConflict = prev })
}

func TestParseSubnetRejectsNonPrivateAndReserved(t *testing.T) {
	for _, cidr := range []string{
		"1.1.0.0/16",     // public
		"8.8.8.0/24",     // public
		"0.0.0.0/0",      // everything
		"127.0.0.0/16",   // loopback
		"169.254.0.0/16", // link local
		"224.0.0.0/24",   // multicast
		"10.0.0.0/8",     // too wide
		"172.0.0.0/12",   // starts outside 172.16.0.0/12
		"192.168.0.0/15", // wider than 192.168.0.0/16
		"10.0.0.0/30",    // too narrow
		"10.53.0.0/24",   // holds the guest DNS address
		"fd00::/64",      // IPv6
	} {
		_, _, err := parseSubnet(cidr)
		require.Error(t, err, cidr)
	}
}

func TestParseSubnetGatewayIsFirstAddress(t *testing.T) {
	for cidr, gw := range map[string]string{
		"192.168.1.128/25": "192.168.1.129",
		"10.20.0.0/16":     "10.20.0.1",
		"172.16.4.8/29":    "172.16.4.9",
		"100.64.3.0/24":    "100.64.3.1",
	} {
		ipNet, got, err := parseSubnet(cidr)
		require.NoError(t, err, cidr)
		require.True(t, ipNet.Contains(got), cidr)
		require.Equal(t, gw, got.String(), cidr)
	}
}

func TestCreateRejectsSubnetOnHostNetwork(t *testing.T) {
	withHostRoute(t, "192.168.1.0/24")
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)

	_, err = s.Create("lan", "192.168.1.0/24", "bridge")
	require.ErrorContains(t, err, "on this host")
	_, err = s.Create("wide", "192.168.0.0/16", "bridge")
	require.ErrorContains(t, err, "on this host")
	_, err = s.Create("other", "192.168.2.0/24", "bridge")
	require.NoError(t, err)
}

func TestAllocateSubnetSkipsHostNetworks(t *testing.T) {
	withHostRoute(t, "10.100.0.0/23")
	_, base, _ := net.ParseCIDR(defaultSubnetCIDR)
	ipNet, gw, err := allocateSubnet(base, nil)
	require.NoError(t, err)
	require.Equal(t, "10.100.2.0/24", ipNet.String())
	require.Equal(t, "10.100.2.1", gw.String())
}

func TestHoldsAddress(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	held, err := s.HoldsAddress(net.ParseIP("10.100.0.5"))
	require.NoError(t, err)
	require.False(t, held, "no networks yet")

	_, err = s.Create("app", "10.100.0.0/24", "bridge")
	require.NoError(t, err)
	for ip, want := range map[string]bool{"10.100.0.5": true, "10.100.0.1": true, "10.100.1.5": false, "192.168.1.20": false} {
		held, err := s.HoldsAddress(net.ParseIP(ip))
		require.NoError(t, err)
		require.Equal(t, want, held, ip)
	}

	// Removing the network is seen at once, not after the cache expires.
	require.NoError(t, s.Remove("app"))
	held, err = s.HoldsAddress(net.ParseIP("10.100.0.5"))
	require.NoError(t, err)
	require.False(t, held)
}

// A store that cannot be read answers an error every time, never a cached
// "not a guest".
func TestHoldsAddressDoesNotCacheAFailedRead(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	_, err = s.Create("app", "10.100.0.0/24", "bridge")
	require.NoError(t, err)
	held, err := s.HoldsAddress(net.ParseIP("10.100.0.5"))
	require.NoError(t, err)
	require.True(t, held)
	require.NoError(t, os.RemoveAll(root))
	require.NoError(t, os.WriteFile(root, nil, 0o600)) // not a directory
	s.gen.Add(1)                                       // as a change made outside the store
	for range 2 {
		_, err := s.HoldsAddress(net.ParseIP("10.100.0.5"))
		require.Error(t, err)
	}
}

// A network created other than through this store (another Store on the same
// directory) is seen once the cache expires.
func TestHoldsAddressSeesOutsideChangesAfterTheTTL(t *testing.T) {
	prev := subnetCacheTTL
	subnetCacheTTL = 50 * time.Millisecond
	t.Cleanup(func() { subnetCacheTTL = prev })
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	other, err := NewStore(root)
	require.NoError(t, err)
	held, err := s.HoldsAddress(net.ParseIP("10.100.0.5"))
	require.NoError(t, err)
	require.False(t, held)

	_, err = other.Create("app", "10.100.0.0/24", "bridge")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		held, err := s.HoldsAddress(net.ParseIP("10.100.0.5"))
		return err == nil && held
	}, 5*time.Second, 10*time.Millisecond)
}
