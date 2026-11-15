//go:build linux || (darwin && arm64)

package network

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What must hold when the store runs out of room, is used by many callers at
// once, or reads files that somebody (a crash, an editor, another version)
// left in a state the code never wrote: no address is ever held by two VMs,
// no address that cannot work is handed out, and a damaged network never
// takes the others down with it.

// drain allocates until the network refuses and returns what it handed out.
func drain(t *testing.T, s *Store, name string) []string {
	t.Helper()
	var out []string
	for range 1 << 16 {
		ip, err := s.AllocateIP(name)
		if err != nil {
			require.ErrorContains(t, err, "no available IPs")
			return out
		}
		out = append(out, ip.String())
	}
	t.Fatal("the network never ran out of addresses")
	return nil
}

func TestExhaustion_HandsOutOnlyUsableHostAddresses(t *testing.T) {
	for _, tc := range []struct {
		cidr string
		want []string
	}{
		// A /29 has eight addresses: network, gateway, five hosts, broadcast.
		{"10.80.0.0/29", []string{"10.80.0.2", "10.80.0.3", "10.80.0.4", "10.80.0.5", "10.80.0.6"}},
		{"10.80.1.8/29", []string{"10.80.1.10", "10.80.1.11", "10.80.1.12", "10.80.1.13", "10.80.1.14"}},
	} {
		t.Run(tc.cidr, func(t *testing.T) {
			s := openNet(t, t.TempDir())
			_, err := s.Create("tiny", tc.cidr, "")
			require.NoError(t, err)
			// The broadcast address must not be given to a VM: a guest
			// configured with it cannot talk to anything (ReserveIP refuses
			// it for the same reason).
			assert.Equal(t, tc.want, drain(t, s, "tiny"))
		})
	}
}

func TestExhaustion_WiderSubnetNeverLeavesItsRange(t *testing.T) {
	s := openNet(t, t.TempDir())
	n, err := s.Create("wide", "10.81.0.0/27", "")
	require.NoError(t, err)
	_, ipNet, err := net.ParseCIDR(n.Subnet)
	require.NoError(t, err)

	got := drain(t, s, "wide")
	require.Len(t, got, 29, "a /27 has 32 addresses less network, gateway and broadcast")
	seen := map[string]bool{}
	for _, ip := range got {
		parsed := net.ParseIP(ip)
		require.True(t, ipNet.Contains(parsed), "%s is outside %s", ip, n.Subnet)
		require.False(t, networkOrBroadcast(ipNet, parsed), "%s is not a host address", ip)
		require.NotEqual(t, n.Gateway, ip)
		require.False(t, seen[ip], "%s handed out twice", ip)
		seen[ip] = true
	}
}

func TestExhaustion_FullNetworkStaysFullAcrossRestartUntilARelease(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	_, err := s.Create("tiny", "10.82.0.0/29", "")
	require.NoError(t, err)
	held := drain(t, s, "tiny")
	require.NotEmpty(t, held)

	// The failed allocation changed nothing: a restarted daemon sees the same
	// full network, and every held address is still refused to a static VM.
	s = openNet(t, root)
	require.NoError(t, s.RecoverReleases(nil))
	_, err = s.AllocateIP("tiny")
	require.ErrorContains(t, err, "no available IPs")
	for _, ip := range held {
		require.ErrorIs(t, s.ReserveIP("tiny", ip), ErrIPAlreadyAllocated)
	}

	// One VM leaves: exactly its address comes back, whichever it was.
	for _, freed := range []string{held[len(held)-1], held[0], held[len(held)/2]} {
		require.NoError(t, s.ReleaseIP("tiny", freed))
		ip, err := s.AllocateIP("tiny")
		require.NoError(t, err)
		assert.Equal(t, freed, ip.String())
		_, err = s.AllocateIP("tiny")
		require.Error(t, err)
	}
}

func TestExhaustion_StaticReservationsCountAgainstTheSubnet(t *testing.T) {
	s := openNet(t, t.TempDir())
	_, err := s.Create("tiny", "10.83.0.0/29", "")
	require.NoError(t, err)
	// Static addresses in the middle and at the end of the range.
	require.NoError(t, s.ReserveIP("tiny", "10.83.0.3"))
	require.NoError(t, s.ReserveIP("tiny", "10.83.0.6"))
	assert.Equal(t, []string{"10.83.0.2", "10.83.0.4", "10.83.0.5"}, drain(t, s, "tiny"))

	require.ErrorContains(t, s.ReserveIP("tiny", "10.83.0.7"), "broadcast")
	require.ErrorContains(t, s.ReserveIP("tiny", "10.83.0.0"), "network or broadcast")
	require.ErrorContains(t, s.ReserveIP("tiny", "10.83.0.8"), "outside")
}

func TestAutoSubnet_Exhausted(t *testing.T) {
	_, base, err := net.ParseCIDR(defaultSubnetCIDR)
	require.NoError(t, err)

	// Every /24 of the default range is taken, one by one.
	var taken []allocatedNet
	for i := range 256 {
		ipNet, gw, err := allocateSubnet(base, taken)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("10.100.%d.0/24", i), ipNet.String())
		require.Equal(t, fmt.Sprintf("10.100.%d.1", i), gw.String())
		taken = append(taken, allocatedNet{ipNet: ipNet, name: fmt.Sprintf("n%d", i)})
	}
	_, _, err = allocateSubnet(base, taken)
	require.ErrorContains(t, err, "no available /24")

	// One network that covers the whole default range leaves no room either,
	// and the failed creation leaves nothing behind.
	root := t.TempDir()
	s := openNet(t, root)
	_, err = s.Create("everything", defaultSubnetCIDR, "")
	require.NoError(t, err)
	_, err = s.Create("auto", "", "")
	require.ErrorContains(t, err, "allocate subnet")
	assert.Equal(t, []string{"everything"}, netNames(t, s))
	_, statErr := os.Stat(filepath.Join(root, "auto"))
	require.True(t, os.IsNotExist(statErr), "a refused network must not leave a directory")

	// Room appears when the big network goes.
	require.NoError(t, s.Remove("everything"))
	n, err := s.Create("auto", "", "")
	require.NoError(t, err)
	assert.Equal(t, "10.100.0.0/24", n.Subnet)
}

func TestAutoSubnet_SkipsHostRoutesUntilNoneIsLeft(t *testing.T) {
	// The host itself routes the whole default range (a VPN, another tool):
	// no automatic subnet may shadow it.
	withHostRoute(t, defaultSubnetCIDR)
	s := openNet(t, t.TempDir())
	_, err := s.Create("auto", "", "")
	require.ErrorContains(t, err, "no available /24")
	assert.Empty(t, netNames(t, s))
}

func TestOverlap_EveryShapeIsRefusedBothWays(t *testing.T) {
	cases := []struct {
		name, first, second string
		overlap             bool
	}{
		{"identical", "10.84.0.0/24", "10.84.0.0/24", true},
		{"second inside first", "10.84.0.0/16", "10.84.7.0/24", true},
		{"first inside second", "10.84.7.0/24", "10.84.0.0/16", true},
		{"last block of the wider one", "10.84.0.0/16", "10.84.255.248/29", true},
		{"same network written with host bits", "10.84.0.0/24", "10.84.0.77/24", true},
		{"adjacent", "10.84.0.0/24", "10.84.1.0/24", false},
		{"adjacent small blocks", "10.84.0.0/29", "10.84.0.8/29", false},
		{"different private ranges", "10.84.0.0/16", "192.168.84.0/24", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openNet(t, t.TempDir())
			_, err := s.Create("first", tc.first, "")
			require.NoError(t, err)
			_, err = s.Create("second", tc.second, "")
			if !tc.overlap {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, `overlaps existing network "first"`)
			assert.Equal(t, []string{"first"}, netNames(t, s))
			// The refusal held nothing: the name works with a free subnet.
			_, err = s.Create("second", "172.20.0.0/24", "")
			require.NoError(t, err)
		})
	}
}

func TestOverlap_UnreadableNetworkDoesNotBlockItsSubnetForever(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	_, err := s.Create("broken", "10.85.0.0/24", "")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "broken", metaFile), nil, 0o600))

	// Its metadata is gone, so nothing says which subnet it had; the store
	// must keep working rather than fail every creation.
	_, err = s.Create("fresh", "10.85.0.0/24", "")
	require.NoError(t, err)
	_, err = s.Create("auto", "", "")
	require.NoError(t, err)
}

func TestRemove_WhileLeasesExist(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	_, err := s.Create("app", "10.86.0.0/24", "")
	require.NoError(t, err)
	first, err := s.AllocateIP("app")
	require.NoError(t, err)
	require.NoError(t, s.ReserveIP("app", "10.86.0.50"))
	require.NoError(t, s.PrepareRelease("app", "vm1", first.String()))

	require.NoError(t, s.Remove("app"))
	_, statErr := os.Stat(filepath.Join(root, "app"))
	require.True(t, os.IsNotExist(statErr))

	// Every later operation on the removed network is an error, never a
	// panic and never a silently recreated half network.
	_, err = s.AllocateIP("app")
	require.ErrorContains(t, err, "not found")
	require.ErrorContains(t, s.ReserveIP("app", "10.86.0.51"), "not found")
	require.ErrorContains(t, s.ReleaseIP("app", first.String()), "not found")
	require.Error(t, s.PrepareRelease("app", "vm2", first.String()))
	require.ErrorContains(t, s.Remove("app"), "not found")
	require.NoError(t, s.RecoverReleases(map[string]bool{"vm1": true}))
	s.ReclaimLeases(map[string][]string{"app": {first.String()}})
	_, statErr = os.Stat(filepath.Join(root, "app"))
	require.True(t, os.IsNotExist(statErr), "an operation on a removed network recreated its directory")

	// A network made again under the name starts clean: the old leases and
	// the old removal intent are gone with it.
	_, err = s.Create("app", "10.86.0.0/24", "")
	require.NoError(t, err)
	require.NoError(t, s.RecoverReleases(nil))
	ip, err := s.AllocateIP("app")
	require.NoError(t, err)
	assert.Equal(t, "10.86.0.2", ip.String())
	require.NoError(t, s.ReserveIP("app", "10.86.0.50"))
}

func TestRemove_HalfRemovedNetworkCanBeRemovedAgain(t *testing.T) {
	// A removal the machine died in the middle of leaves the directory with
	// some of its files. Whichever are left, the name is not lost.
	for _, left := range []string{metaFile, stateFile, ""} {
		t.Run("left="+left, func(t *testing.T) {
			root := t.TempDir()
			s := openNet(t, root)
			_, err := s.Create("app", "10.87.0.0/24", "")
			require.NoError(t, err)
			for _, f := range []string{metaFile, stateFile} {
				if f != left {
					require.NoError(t, os.Remove(filepath.Join(root, "app", f)))
				}
			}

			s = openNet(t, root)
			require.NoError(t, s.RecoverReleases(nil))
			require.NoError(t, s.Remove("app"))
			assert.Empty(t, netNames(t, s))
			_, err = s.Create("app", "10.87.0.0/24", "")
			require.NoError(t, err)
		})
	}
}

func TestStore_RootDirectoryProblems(t *testing.T) {
	t.Run("root is a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "networks")
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
		_, err := NewStore(path)
		require.Error(t, err)
	})
	t.Run("root deleted under a running store", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "networks")
		s := openNet(t, root)
		_, err := s.Create("app", "10.88.0.0/24", "")
		require.NoError(t, err)
		require.NoError(t, os.RemoveAll(root))

		_, err = s.Create("other", "10.88.1.0/24", "")
		require.Error(t, err)
		_, err = s.List()
		require.Error(t, err)
		require.Error(t, s.RecoverReleases(nil))
		_, err = s.AllocateIP("app")
		require.Error(t, err)
		require.Error(t, s.Remove("app"))
	})
	t.Run("root read-only", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		root := filepath.Join(t.TempDir(), "networks")
		s := openNet(t, root)
		_, err := s.Create("app", "10.89.0.0/24", "")
		require.NoError(t, err)
		held, err := s.AllocateIP("app")
		require.NoError(t, err)
		require.NoError(t, os.Chmod(root, 0o500))
		require.NoError(t, os.Chmod(filepath.Join(root, "app"), 0o500))
		t.Cleanup(func() {
			_ = os.Chmod(root, 0o700)                       //nolint:gosec // G302: a directory needs its search bit.
			_ = os.Chmod(filepath.Join(root, "app"), 0o700) //nolint:gosec // G302: as above.
		})

		// Nothing can be written: every change is reported, none is half done.
		_, err = s.Create("other", "10.89.1.0/24", "")
		require.Error(t, err)
		_, err = s.AllocateIP("app")
		require.Error(t, err)
		require.Error(t, s.ReserveIP("app", "10.89.0.9"))
		require.Error(t, s.ReleaseIP("app", held.String()))
		assert.Equal(t, []string{"app"}, netNames(t, s))

		// The disk becomes writable again and the state is the one from before.
		require.NoError(t, os.Chmod(root, 0o700))                       //nolint:gosec // G302: a directory needs its search bit.
		require.NoError(t, os.Chmod(filepath.Join(root, "app"), 0o700)) //nolint:gosec // G302: as above.
		require.ErrorIs(t, s.ReserveIP("app", held.String()), ErrIPAlreadyAllocated)
		ip, err := s.AllocateIP("app")
		require.NoError(t, err)
		assert.Equal(t, "10.89.0.3", ip.String())
	})
}

// metaDamage lists what a metadata file can hold that the store never wrote:
// what a power cut leaves, and well-formed JSON with values that make no sense.
var metaDamage = map[string]string{
	"empty":            ``,
	"truncated":        `{"id":"app","name":"app","subnet":"10.9`,
	"zeroes":           string(make([]byte, 128)),
	"garbage":          "\xff\xfe\x00\x13",
	"json null":        `null`,
	"json array":       `[]`,
	"json string":      `"app"`,
	"no subnet":        `{"id":"app","name":"app"}`,
	"subnet garbage":   `{"id":"app","name":"app","subnet":"not-a-cidr","gateway":"x"}`,
	"subnet ipv6":      `{"id":"app","name":"app","subnet":"fd00::/64","gateway":"fd00::1"}`,
	"subnet /32":       `{"id":"app","name":"app","subnet":"10.90.0.5/32","gateway":"10.90.0.5"}`,
	"subnet /0":        `{"id":"app","name":"app","subnet":"0.0.0.0/0","gateway":"0.0.0.1"}`,
	"wrong types":      `{"id":7,"name":["app"],"subnet":{},"gateway":null}`,
	"name is a path":   `{"id":"../x","name":"../../etc","subnet":"10.90.0.0/24","gateway":"10.90.0.1"}`,
	"huge string":      `{"name":"app","subnet":"` + longDigits(70000) + `"}`,
	"other network":    `{"id":"fine","name":"fine","subnet":"10.91.0.0/24","gateway":"10.91.0.1"}`,
	"empty name":       `{"subnet":"10.90.0.0/24","gateway":"10.90.0.1"}`,
	"trailing garbage": `{"id":"app","name":"app","subnet":"10.90.0.0/24","gateway":"10.90.0.1"}}}}`,
}

func longDigits(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '1'
	}
	return string(b)
}

func TestDamagedMetadata_NeverPanicsAndNeverSpreads(t *testing.T) {
	for name, content := range metaDamage {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s := openNet(t, root)
			_, err := s.Create("app", "10.90.0.0/24", "")
			require.NoError(t, err)
			_, err = s.Create("fine", "10.91.0.0/24", "")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, "app", metaFile), []byte(content), 0o600))

			s = openNet(t, root)
			// Whatever the file holds, every entry point returns; an address
			// handed out for the damaged network must at least be an address
			// of the subnet its metadata now claims.
			require.NoError(t, s.RecoverReleases(nil))
			if n, gerr := s.Get("app"); gerr == nil {
				if ip, aerr := s.AllocateIP("app"); aerr == nil {
					_, ipNet, perr := net.ParseCIDR(n.Subnet)
					require.NoError(t, perr)
					require.True(t, ipNet.Contains(ip), "%s is outside %s", ip, n.Subnet)
				}
			}
			_ = s.ReserveIP("app", "10.90.0.9")
			_ = s.ReleaseIP("app", "10.90.0.9")
			_ = s.PrepareRelease("app", "vm", "10.90.0.9")
			s.ReclaimLeases(map[string][]string{"app": {"10.90.0.10"}})

			// The healthy network is untouched and new networks can be made.
			assert.Contains(t, netNames(t, s), "fine")
			ip, err := s.AllocateIP("fine")
			require.NoError(t, err)
			assert.Equal(t, "10.91.0.2", ip.String())
			// A new network is refused only when the damaged metadata still
			// reads as a network whose subnet covers it, and says which.
			if _, err = s.Create("later", "10.92.0.0/24", ""); err != nil {
				require.ErrorContains(t, err, `overlaps existing network "app"`)
			}

			// And the damaged one can always be removed and made again,
			// which also frees whatever range it claimed.
			require.NoError(t, s.Remove("app"))
			_, err = s.Create("app", "10.90.0.0/24", "")
			require.NoError(t, err)
			if _, gerr := s.Get("later"); gerr != nil {
				_, err = s.Create("later", "10.92.0.0/24", "")
				require.NoError(t, err)
			}
		})
	}
}

func TestDamagedLeaseState_WellFormedButWrong(t *testing.T) {
	// Lease files that parse but hold values the store never writes. None may
	// make the store panic or hand out an address outside the subnet, the
	// gateway, or an address the file lists as taken.
	for name, content := range map[string]string{
		"null":             `null`,
		"empty object":     `{}`,
		"index zero":       `{"allocated_ips":["10.93.0.1"],"next_index":0}`,
		"index negative":   `{"allocated_ips":["10.93.0.1"],"next_index":-5}`,
		"index huge":       `{"allocated_ips":["10.93.0.1"],"next_index":9223372036854775807}`,
		"index past range": `{"allocated_ips":["10.93.0.1"],"next_index":4096}`,
		"junk addresses":   `{"allocated_ips":["","x","10.93.0.300","fe80::1","10.93.0.2"],"next_index":2}`,
		"null pending":     `{"allocated_ips":["10.93.0.1","10.93.0.2"],"next_index":3,"pending_releases":null}`,
		"pending unknown":  `{"allocated_ips":["10.93.0.1","10.93.0.2"],"next_index":3,"pending_releases":{"ghost":"10.93.0.77","":""}}`,
		"gateway missing":  `{"allocated_ips":[],"next_index":1}`,
		"duplicates":       `{"allocated_ips":["10.93.0.1","10.93.0.2","10.93.0.2","10.93.0.2"],"next_index":2}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s := openNet(t, root)
			n, err := s.Create("app", "10.93.0.0/28", "")
			require.NoError(t, err)
			_, ipNet, err := net.ParseCIDR(n.Subnet)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, "app", stateFile), []byte(content), 0o600))

			s = openNet(t, root)
			require.NoError(t, s.RecoverReleases(nil))
			st, err := s.readState("app")
			require.NoError(t, err)
			listed := map[string]bool{}
			for _, ip := range st.AllocatedIPs {
				listed[ip] = true
			}

			seen := map[string]bool{}
			for range 64 {
				ip, err := s.AllocateIP("app")
				if err != nil {
					break
				}
				require.True(t, ipNet.Contains(ip), "%s is outside %s", ip, n.Subnet)
				require.False(t, networkOrBroadcast(ipNet, ip), "%s is not a host address", ip)
				require.NotEqual(t, n.Gateway, ip.String(), "the gateway was handed to a VM")
				require.False(t, listed[ip.String()], "%s was listed as taken", ip)
				require.False(t, seen[ip.String()], "%s handed out twice", ip)
				seen[ip.String()] = true
			}
			require.LessOrEqual(t, len(seen), 13, "a /28 has 13 addresses for VMs")
		})
	}
}

func TestReserveIP_AddressSpellings(t *testing.T) {
	s := openNet(t, t.TempDir())
	_, err := s.Create("app", "10.94.0.0/24", "")
	require.NoError(t, err)
	require.NoError(t, s.ReserveIP("app", "10.94.0.5"))

	// The same address written another way is the same address.
	require.ErrorIs(t, s.ReserveIP("app", "::ffff:10.94.0.5"), ErrIPAlreadyAllocated)
	require.ErrorIs(t, s.ReserveIP("app", "::ffff:10.94.0.1"), ErrIPAlreadyAllocated, "the gateway is taken")

	for _, bad := range []string{
		"", " ", "10.94.0", "10.94.0.5.6", "10.94.0.256", "10.94.0.5/24", " 10.94.0.6", "10.94.0.6 ",
		"10.94.0.6\n", "fd00::5", "::1", "0x0a5e0006", "localhost", "10.94.0.-1", "١٠.٩٤.٠.٦",
	} {
		err := s.ReserveIP("app", bad)
		require.Error(t, err, "%q", bad)
		require.NotErrorIs(t, err, ErrIPAlreadyAllocated, "%q", bad)
	}
	// None of the refusals took an address.
	ip, err := s.AllocateIP("app")
	require.NoError(t, err)
	assert.Equal(t, "10.94.0.2", ip.String())
}

func TestStore_HostileNetworkNames(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	outside := filepath.Join(filepath.Dir(root), "victim")
	require.NoError(t, os.MkdirAll(outside, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(outside, stateFile), []byte("keep"), 0o600))
	t.Cleanup(func() { _ = os.RemoveAll(outside) })

	for _, name := range []string{
		"", ".", "..", "../victim", "..\\victim", "a/b", "/abs", "a\x00b", "a b", "-leading", "app\n",
		"../" + filepath.Base(root) + "/x", longDigits(300),
	} {
		_, err := s.Create(name, "10.95.0.0/24", "")
		require.Error(t, err, "create %q", name)
		_, err = s.Get(name)
		require.Error(t, err, "get %q", name)
		_, err = s.AllocateIP(name)
		require.Error(t, err, "allocate %q", name)
		require.Error(t, s.ReserveIP(name, "10.95.0.5"), "reserve %q", name)
		require.Error(t, s.ReleaseIP(name, "10.95.0.5"), "release %q", name)
		require.Error(t, s.PrepareRelease(name, "vm", "10.95.0.5"), "prepare %q", name)
		require.Error(t, s.Remove(name), "remove %q", name)
	}
	assert.Empty(t, netNames(t, s))
	data, err := os.ReadFile(filepath.Join(outside, stateFile))
	require.NoError(t, err, "a name escaped the store and removed a neighboring directory")
	assert.Equal(t, "keep", string(data))
}

func TestRelease_PreparedThenReleasedAddressIsNotFreedTwice(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	_, err := s.Create("app", "10.96.0.0/24", "")
	require.NoError(t, err)
	ip, err := s.AllocateIP("app")
	require.NoError(t, err)

	// vm-old is removed cleanly: intent recorded, address released. A new VM
	// then gets the same address, and the daemon restarts.
	require.NoError(t, s.PrepareRelease("app", "vm-old", ip.String()))
	require.NoError(t, s.ReleaseIP("app", ip.String()))
	again, err := s.AllocateIP("app")
	require.NoError(t, err)
	require.Equal(t, ip.String(), again.String())

	s = openNet(t, root)
	require.NoError(t, s.RecoverReleases(map[string]bool{"vm-new": true}))
	// The stale intent of vm-old must not take the address from vm-new.
	require.ErrorIs(t, s.ReserveIP("app", ip.String()), ErrIPAlreadyAllocated)
	next, err := s.AllocateIP("app")
	require.NoError(t, err)
	assert.NotEqual(t, ip.String(), next.String())
}

func TestRelease_IntentForAnAddressNobodyHoldsIsIgnored(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	_, err := s.Create("app", "10.97.0.0/24", "")
	require.NoError(t, err)
	require.NoError(t, s.PrepareRelease("app", "vm1", "10.97.0.9"))
	require.NoError(t, s.PrepareRelease("app", "vm1", "garbage"))
	require.NoError(t, s.ReleaseIP("app", "10.97.0.9"), "releasing a free address is not an error")
	require.NoError(t, s.ReleaseIP("app", "garbage"))

	st, err := s.readState("app")
	require.NoError(t, err)
	assert.Empty(t, st.PendingReleases)
	assert.Equal(t, []string{"10.97.0.1"}, st.AllocatedIPs)
}

func TestFaults_RecoverReleasesFailingIsRetried(t *testing.T) {
	for _, f := range durabletest.Failures() {
		if f.Step == durable.StepSync || f.Step == durable.StepSyncDir {
			continue // lease state is not flushed, so those steps never run
		}
		t.Run(f.Name, func(t *testing.T) {
			root := t.TempDir()
			s := openNet(t, root)
			_, err := s.Create("app", "10.98.0.0/24", "")
			require.NoError(t, err)
			gone, err := s.AllocateIP("app")
			require.NoError(t, err)
			alive, err := s.AllocateIP("app")
			require.NoError(t, err)
			require.NoError(t, s.PrepareRelease("app", "vm-gone", gone.String()))
			require.NoError(t, s.PrepareRelease("app", "vm-alive", alive.String()))

			// The daemon restarts and the disk fails while it finishes the
			// interrupted removals.
			live := map[string]bool{"vm-alive": true}
			s = openNet(t, root)
			inj := durabletest.Inject(t, f, 0, nil)
			err = s.RecoverReleases(live)
			require.True(t, inj.Fired())
			require.Error(t, err, "a recovery that was not saved must be reported")
			durable.SetFault(nil)

			// Starting again finishes the job: the intents were not lost and
			// the surviving VM never lost its address.
			s = openNet(t, root)
			require.NoError(t, s.RecoverReleases(live))
			require.ErrorIs(t, s.ReserveIP("app", alive.String()), ErrIPAlreadyAllocated)
			require.NoError(t, s.ReserveIP("app", gone.String()))
		})
	}
}

func TestFaults_LeftoverTemporaryFilesAreHarmless(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	_, err := s.Create("app", "10.99.0.0/24", "")
	require.NoError(t, err)
	// A crash at every step of a lease write, several times over.
	for _, f := range durabletest.Failures() {
		if !f.Crash() || f.Step == durable.StepSync || f.Step == durable.StepSyncDir {
			continue
		}
		durabletest.Inject(t, f, 0, nil)
		_, _ = s.AllocateIP("app")
		durable.SetFault(nil)
	}
	// And one in the store's root, where networks are listed.
	require.NoError(t, os.WriteFile(filepath.Join(root, ".app.tmp-123"), []byte("{"), 0o600))

	s = openNet(t, root)
	require.NoError(t, s.RecoverReleases(nil))
	assert.Equal(t, []string{"app"}, netNames(t, s))
	seen := map[string]bool{}
	for range 10 {
		ip, err := s.AllocateIP("app")
		require.NoError(t, err)
		require.False(t, seen[ip.String()])
		seen[ip.String()] = true
	}
	require.NoError(t, s.Remove("app"), "leftover temporary files must not keep a network from being removed")
	_, statErr := os.Stat(filepath.Join(root, "app"))
	require.True(t, os.IsNotExist(statErr))
}

func TestConcurrent_AllocateNeverHandsOutADuplicate(t *testing.T) {
	s := openNet(t, t.TempDir())
	_, err := s.Create("app", "10.101.0.0/24", "")
	require.NoError(t, err)

	const workers, each = 16, 12
	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				ip, err := s.AllocateIP("app")
				if !assert.NoError(t, err) {
					return
				}
				mu.Lock()
				seen[ip.String()]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Len(t, seen, workers*each)
	for ip, n := range seen {
		require.Equal(t, 1, n, "%s was handed out %d times", ip, n)
	}
}

func TestConcurrent_AllocateAndReleaseNeverShareAnAddress(t *testing.T) {
	s := openNet(t, t.TempDir())
	_, err := s.Create("app", "10.102.0.0/27", "") // small on purpose: addresses are reused constantly
	require.NoError(t, err)

	// Each worker is a VM that takes an address, checks nobody else holds it,
	// and returns it. Static reservations compete for the same range.
	var (
		mu      sync.Mutex
		holders = map[string]int{}
		wg      sync.WaitGroup
	)
	hold := func(ip string, worker int) {
		mu.Lock()
		defer mu.Unlock()
		if other, taken := holders[ip]; taken {
			t.Errorf("%s given to worker %d while worker %d holds it", ip, worker, other)
		}
		holders[ip] = worker
	}
	drop := func(ip string) {
		mu.Lock()
		delete(holders, ip)
		mu.Unlock()
	}
	for w := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 40 {
				var ip string
				if w%4 == 0 {
					// A VM with a static address: it either gets exactly
					// that address or a conflict, never a silent overlap.
					ip = fmt.Sprintf("10.102.0.%d", 2+(w+i)%29)
					if err := s.ReserveIP("app", ip); err != nil {
						assert.ErrorIs(t, err, ErrIPAlreadyAllocated)
						continue
					}
				} else {
					got, err := s.AllocateIP("app")
					if err != nil {
						assert.ErrorContains(t, err, "no available IPs")
						continue
					}
					ip = got.String()
				}
				hold(ip, w)
				if i%3 == 0 {
					assert.NoError(t, s.PrepareRelease("app", fmt.Sprintf("vm-%d-%d", w, i), ip))
				}
				drop(ip)
				assert.NoError(t, s.ReleaseIP("app", ip))
			}
		}()
	}
	wg.Wait()

	// Everything was returned: the whole range is available again, and no
	// removal intent outlived its release.
	st, err := s.readState("app")
	require.NoError(t, err)
	assert.Equal(t, []string{"10.102.0.1"}, st.AllocatedIPs)
	assert.Empty(t, st.PendingReleases)
	assert.Len(t, drain(t, s, "app"), 29)
}

func TestConcurrent_CreateSameNameHasOneWinner(t *testing.T) {
	s := openNet(t, t.TempDir())
	const callers = 12
	errs := make([]error, callers)
	nets := make([]*Network, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nets[i], errs[i] = s.Create("app", "", "")
		}()
	}
	wg.Wait()
	won := 0
	for i, err := range errs {
		if err == nil {
			won++
			require.NotNil(t, nets[i])
			continue
		}
		require.ErrorContains(t, err, "already exists")
	}
	require.Equal(t, 1, won)
	assert.Equal(t, []string{"app"}, netNames(t, s))
}

func TestConcurrent_AutoSubnetsNeverOverlap(t *testing.T) {
	s := openNet(t, t.TempDir())
	const callers = 16
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Create(fmt.Sprintf("net%d", i), "", "")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, callers)
	var subnets []*net.IPNet
	for _, n := range list {
		_, ipNet, err := net.ParseCIDR(n.Subnet)
		require.NoError(t, err)
		for _, other := range subnets {
			require.False(t, subnetsOverlap(ipNet, other), "%s overlaps %s", ipNet, other)
		}
		subnets = append(subnets, ipNet)
	}
}

func TestConcurrent_RemoveWhileOthersUseTheNetwork(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	_, err := s.Create("stable", "10.103.0.0/24", "")
	require.NoError(t, err)

	// One caller removes and recreates a network while others allocate from
	// it, list and read. Callers that lose the race get an error; nobody gets
	// a panic, a torn read, or an address from the wrong network.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for range 25 {
			if _, err := s.Create("flaky", "10.104.0.0/24", ""); !assert.NoError(t, err) {
				return
			}
			if !assert.NoError(t, s.Remove("flaky")) {
				return
			}
		}
	}()
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if ip, err := s.AllocateIP("flaky"); err == nil {
					assert.Equal(t, "10.104.0", ip.String()[:8])
					_ = s.ReleaseIP("flaky", ip.String())
				}
				list, err := s.List()
				assert.NoError(t, err)
				for _, n := range list {
					assert.NotEmpty(t, n.Name, "a half-written network was listed")
					assert.NotEmpty(t, n.Subnet)
				}
				if n, err := s.Get("stable"); assert.NoError(t, err) {
					assert.Equal(t, "10.103.0.0/24", n.Subnet)
				}
			}
		}()
	}
	wg.Wait()

	_, statErr := os.Stat(filepath.Join(root, "flaky"))
	require.True(t, os.IsNotExist(statErr), "an operation that lost the race recreated the removed network")
	ip, err := s.AllocateIP("stable")
	require.NoError(t, err)
	assert.Equal(t, "10.103.0.2", ip.String())
}

func FuzzParseSubnet(f *testing.F) {
	for _, seed := range []string{
		"10.0.0.0/24", "10.100.0.0/16", "172.16.5.0/29", "192.168.1.77/24", "100.64.0.0/10",
		"", "/", "10.0.0.0", "10.0.0.0/", "10.0.0.0/33", "10.0.0.0/-1", "10.0.0.0/08", "8.8.8.0/24",
		"fd00::/64", "::ffff:10.0.0.0/120", "10.53.0.0/24", "0.0.0.0/0", "10.0.0.0/24/24", "10.0.0.0/24 ",
		"256.0.0.0/24", "10.0.0.0/30", "10.0.0.0/15", "1e3.0.0.0/24", "\x00/24",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, cidr string) {
		ipNet, gw, err := parseSubnet(cidr)
		if err != nil {
			return
		}
		// Whatever is accepted becomes an address and a route on the host,
		// so it has to be a private IPv4 block of a sane size whose gateway
		// is a host address inside it.
		ones, bits := ipNet.Mask.Size()
		if bits != 32 || ones < minSubnetPrefix || ones > maxSubnetPrefix {
			t.Fatalf("%q accepted with prefix /%d of %d bits", cidr, ones, bits)
		}
		if !inPrivateRange(ipNet) {
			t.Fatalf("%q accepted outside the private ranges", cidr)
		}
		if gw.To4() == nil || !ipNet.Contains(gw) || networkOrBroadcast(ipNet, gw) {
			t.Fatalf("%q accepted with gateway %s", cidr, gw)
		}
		// The first address a VM gets is a usable one too.
		first := nextIP(ipNet, 2)
		if first == nil || networkOrBroadcast(ipNet, first) || first.Equal(gw) {
			t.Fatalf("%q accepted but its first VM address is %v", cidr, first)
		}
		// Accepting is stable: the canonical form parses to the same block.
		again, gw2, err := parseSubnet(ipNet.String())
		if err != nil || again.String() != ipNet.String() || !gw2.Equal(gw) {
			t.Fatalf("%q -> %s does not parse back: %v", cidr, ipNet, err)
		}
	})
}
