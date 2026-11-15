//go:build linux

package network

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// sysctls records route_localnet writes instead of touching the host.
type sysctls struct {
	mu     sync.Mutex
	values map[string]string
	writes []string
	gone   map[string]bool
}

func fakeSysctls(t *testing.T) *sysctls {
	t.Helper()
	s := &sysctls{values: map[string]string{}, gone: map[string]bool{}}
	prevWrite, prevGuard := writeRouteLocalnet, loopbackGuard
	writeRouteLocalnet = func(bridge, value string) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.gone[bridge] {
			return os.ErrNotExist
		}
		s.values[bridge] = value
		s.writes = append(s.writes, bridge+"="+value)
		return nil
	}
	localnet.mu.Lock()
	prevBridges := localnet.bridges
	localnet.bridges = map[string]bool{}
	localnet.mu.Unlock()
	t.Cleanup(func() {
		writeRouteLocalnet, loopbackGuard = prevWrite, prevGuard
		localnet.mu.Lock()
		localnet.bridges = prevBridges
		localnet.mu.Unlock()
	})
	return s
}

func (s *sysctls) value(bridge string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[bridge]
}

// fakeGuard replaces the nft calls of the guard; loaded is what the kernel
// probe reports, and a successful script loads the guard.
type fakeGuardKernel struct {
	mu      sync.Mutex
	loaded  bool
	failNft bool
	scripts []string
}

func withFakeGuard(t *testing.T, k *fakeGuardKernel, dnsIP string, installed bool) {
	t.Helper()
	prevNft, prevProbe := guardNft, guardProbe
	guardNft = func(script string) error {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.scripts = append(k.scripts, script)
		if k.failNft {
			return errors.New("nft: injected failure")
		}
		if strings.Contains(script, "delete table inet "+guardTable) {
			k.loaded = true
		}
		return nil
	}
	guardProbe = func() error {
		k.mu.Lock()
		defer k.mu.Unlock()
		if !k.loaded {
			return errors.New("guard: tables not loaded")
		}
		return nil
	}
	guard.mu.Lock()
	prevInstalled, prevDNS, prevBridges, prevTaps := guard.installed, guard.dnsIP, guard.bridges, guard.taps
	guard.installed, guard.dnsIP, guard.bridges, guard.taps = installed, dnsIP, map[string]bool{}, map[string]string{}
	guard.mu.Unlock()
	t.Cleanup(func() {
		guardNft, guardProbe = prevNft, prevProbe
		guard.mu.Lock()
		guard.installed, guard.dnsIP, guard.bridges, guard.taps = prevInstalled, prevDNS, prevBridges, prevTaps
		guard.mu.Unlock()
	})
}

// Without the loopback guard a bridge never gets route_localnet: the ports
// stay on the userspace proxy, and an earlier 1 is turned off.
func TestPrepareBridgeFailsClosedWithoutGuard(t *testing.T) {
	s := fakeSysctls(t)
	loopbackGuard = func() error { return errors.New("guard: tables not loaded") }
	err := nftNAT{}.prepareBridge("jerboa-br-app")
	require.ErrorContains(t, err, "loopback guard")
	require.Equal(t, []string{"jerboa-br-app=0"}, s.writes)

	loopbackGuard = func() error { return nil }
	require.NoError(t, nftNAT{}.prepareBridge("jerboa-br-app"))
	require.Equal(t, "1", s.value("jerboa-br-app"))
}

// Publishing with kernel NAT while the guard is gone falls back to the proxy
// instead of exposing the host's loopback.
func TestPublishPortsWithoutGuardUsesTheProxy(t *testing.T) {
	s := fakeSysctls(t)
	loopbackGuard = func() error { return errors.New("guard: tables not loaded") }
	n := &guardedNAT{}
	withNAT(t, n)
	f, err := PublishPorts("10.100.0.9", "jerboa-br0", []PortForward{{HostPort: freePort(t, "tcp"), GuestPort: 80, Protocol: "tcp", BindAddr: "127.0.0.1"}})
	require.NoError(t, err)
	defer f.Close()
	require.Empty(t, n.added, "kernel NAT was used without the guard")
	require.Equal(t, "0", s.value("jerboa-br0"))
}

// guardedNAT is nftNAT's prepareBridge with the nft calls recorded.
type guardedNAT struct {
	fakeNAT
}

func (g *guardedNAT) prepareBridge(b string) error { return nftNAT{}.prepareBridge(b) }

// The guard tables were flushed behind the daemon: EnsureGuard reinstalls
// them with the bindings of the guests that are running.
func TestEnsureGuardReinstallsRemovedTables(t *testing.T) {
	k := &fakeGuardKernel{loaded: true}
	withFakeGuard(t, k, "10.53.0.53", true)
	require.NoError(t, GuardTap("tap-a", "52:54:00:00:00:02", "10.100.0.2"))
	require.NoError(t, EnsureGuard())
	k.mu.Lock()
	scripts := len(k.scripts)
	k.loaded = false // nft flush ruleset
	k.mu.Unlock()

	require.NoError(t, EnsureGuard())
	k.mu.Lock()
	defer k.mu.Unlock()
	require.True(t, k.loaded)
	require.Len(t, k.scripts, scripts+1)
	reinstall := k.scripts[len(k.scripts)-1]
	require.Contains(t, reinstall, `iifname != "lo" ip daddr 127.0.0.0/8 drop`)
	require.Contains(t, reinstall, "ip daddr 10.53.0.53 drop")
	require.Contains(t, reinstall, `add element bridge jerboa_guard tap_ip { "tap-a" . 10.100.0.2 }`)
	require.Contains(t, reinstall, `add element bridge jerboa_guard taps { "tap-a" }`)
}

// A guest that stopped is not bound again, and a guard that was never
// installed is installed once nft works, with the guests started meanwhile.
func TestEnsureGuardInstallsLateAndForgetsStoppedGuests(t *testing.T) {
	k := &fakeGuardKernel{failNft: true}
	withFakeGuard(t, k, "10.53.0.53", false)
	require.NoError(t, GuardTap("tap-a", "52:54:00:00:00:02", "10.100.0.2"))
	require.NoError(t, GuardTap("tap-b", "52:54:00:00:00:03", "10.100.0.3"))
	UnguardTap("tap-b", "52:54:00:00:00:03", "10.100.0.3")
	require.Error(t, EnsureGuard())

	k.mu.Lock()
	k.failNft = false
	k.mu.Unlock()
	require.NoError(t, EnsureGuard())
	k.mu.Lock()
	defer k.mu.Unlock()
	last := k.scripts[len(k.scripts)-1]
	require.Contains(t, last, `"tap-a"`)
	require.NotContains(t, last, `"tap-b"`)
}

func TestEnsureGuardNeedsInstallGuard(t *testing.T) {
	withFakeGuard(t, &fakeGuardKernel{loaded: true}, "", false)
	require.ErrorContains(t, EnsureGuard(), "not configured")
}

// The watchdog turns route_localnet off while the guard cannot be loaded and
// back on once it is; bridges that are gone are forgotten.
func TestCheckGuardTogglesRouteLocalnet(t *testing.T) {
	s := fakeSysctls(t)
	loopbackGuard = func() error { return nil }
	require.NoError(t, nftNAT{}.prepareBridge("jerboa-br-a"))
	require.NoError(t, nftNAT{}.prepareBridge("jerboa-br-b"))

	checkGuard() // guard loaded: nothing changes
	require.Equal(t, "1", s.value("jerboa-br-a"))

	loopbackGuard = func() error { return errors.New("guard: tables not loaded") }
	checkGuard()
	require.Equal(t, "0", s.value("jerboa-br-a"))
	require.Equal(t, "0", s.value("jerboa-br-b"))

	s.mu.Lock()
	s.gone["jerboa-br-b"] = true
	s.mu.Unlock()
	loopbackGuard = func() error { return nil }
	checkGuard()
	require.Equal(t, "1", s.value("jerboa-br-a"))
	localnet.mu.Lock()
	defer localnet.mu.Unlock()
	require.Equal(t, map[string]bool{"jerboa-br-a": true}, localnet.bridges)
}
