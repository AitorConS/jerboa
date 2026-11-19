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
	prevNft, prevProbe, prevHasNft := guardNft, guardProbe, guardHasNft
	guardHasNft = func() bool { return true }
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
	prevAllow, prevNoBridge, prevHostAccess := guard.allowUnguarded, guard.noBridge, guard.hostAccess
	guard.installed, guard.dnsIP, guard.bridges, guard.taps = installed, dnsIP, map[string]bool{}, map[string]string{}
	guard.allowUnguarded, guard.noBridge, guard.hostAccess = false, false, false
	guard.mu.Unlock()
	t.Cleanup(func() {
		guardNft, guardProbe, guardHasNft = prevNft, prevProbe, prevHasNft
		guard.mu.Lock()
		guard.installed, guard.dnsIP, guard.bridges, guard.taps = prevInstalled, prevDNS, prevBridges, prevTaps
		guard.allowUnguarded, guard.noBridge, guard.hostAccess = prevAllow, prevNoBridge, prevHostAccess
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

// Without nft the guard cannot load, and a guest is not attached to a bridge:
// the error says what to install and how to opt out.
func TestRequireGuardFailsClosedWithoutNft(t *testing.T) {
	k := &fakeGuardKernel{}
	withFakeGuard(t, k, "10.53.0.53", false)
	guardHasNft = func() bool { return false }
	err := RequireGuard()
	require.ErrorContains(t, err, "nft not found")
	require.ErrorContains(t, err, "sudo apt-get install nftables")
	require.ErrorContains(t, err, AllowUnguardedEnv+"=1")
	require.Empty(t, k.scripts, "nft was run although it is not installed")
	st := CheckGuard()
	require.True(t, st.Configured)
	require.True(t, st.Enforced)
	require.ErrorIs(t, st.Err, errNoNft)
}

// A ruleset the kernel refuses is as good as no guard.
func TestRequireGuardFailsClosedWhenTheRulesetIsRefused(t *testing.T) {
	k := &fakeGuardKernel{failNft: true}
	withFakeGuard(t, k, "10.53.0.53", false)
	require.ErrorContains(t, RequireGuard(), "injected failure")
}

// A daemon that never configured the guard does not attach guests either.
func TestRequireGuardNeedsInstallGuard(t *testing.T) {
	withFakeGuard(t, &fakeGuardKernel{loaded: true}, "", false)
	require.ErrorContains(t, RequireGuard(), "not configured")
	require.False(t, CheckGuard().Configured)
}

// nftables installed after the daemon started: the next VM loads the guard
// instead of failing.
func TestRequireGuardLoadsTheGuardLate(t *testing.T) {
	k := &fakeGuardKernel{}
	withFakeGuard(t, k, "10.53.0.53", false)
	guardHasNft = func() bool { return false }
	require.Error(t, RequireGuard())
	guardHasNft = func() bool { return true }
	require.NoError(t, RequireGuard())
	// CheckGuard probes the fake kernel, which takes k.mu: read loaded first
	// and release the lock before calling it.
	k.mu.Lock()
	loaded := k.loaded
	k.mu.Unlock()
	require.True(t, loaded)
	require.NoError(t, CheckGuard().Err)
}

// The operator's opt-out lets the guest start without the guard.
func TestRequireGuardOptOut(t *testing.T) {
	withFakeGuard(t, &fakeGuardKernel{}, "10.53.0.53", false)
	guardHasNft = func() bool { return false }
	guard.mu.Lock()
	guard.allowUnguarded = true
	guard.mu.Unlock()
	require.NoError(t, RequireGuard())
	st := CheckGuard()
	require.False(t, st.Enforced)
	require.True(t, st.AllowUnguarded)
	require.Error(t, st.Err)
}

// InstallGuard records the opt-out and the missing nft, so the guard can be
// loaded later.
func TestInstallGuardWithoutNftKeepsTheConfiguration(t *testing.T) {
	withFakeGuard(t, &fakeGuardKernel{}, "", false)
	guardHasNft = func() bool { return false }
	prevWSL := guardIsWSL
	guardIsWSL = func() bool { return false }
	t.Cleanup(func() { guardIsWSL = prevWSL })
	require.ErrorIs(t, InstallGuard("10.53.0.53", GuardOptions{AllowUnguarded: true}), errNoNft)
	st := CheckGuard()
	require.True(t, st.Configured)
	require.True(t, st.AllowUnguarded)
	require.False(t, st.Enforced)
}

// On WSL (no nftables bridge family) only the inet table is installed, tap
// bindings never reach nft, the probe does not look for the bridge table,
// and a guard that cannot load does not block guests.
func TestGuardOnWSLLeavesTheBridgeFamilyOut(t *testing.T) {
	k := &fakeGuardKernel{}
	withFakeGuard(t, k, "", false)
	prevWSL, prevList := guardIsWSL, guardList
	guardIsWSL = func() bool { return true }
	var listed []string
	guardList = func(script string) (string, error) {
		listed = append(listed, script)
		return `iifname != "lo" ip daddr 127.0.0.0/8 drop`, nil
	}
	t.Cleanup(func() { guardIsWSL, guardList = prevWSL, prevList })

	require.NoError(t, InstallGuard("10.53.0.53", GuardOptions{}))
	require.NoError(t, GuardTap("tap-a", "52:54:00:00:00:02", "10.100.0.2"))
	UnguardTap("tap-a", "52:54:00:00:00:02", "10.100.0.2")
	k.mu.Lock()
	require.Len(t, k.scripts, 1)
	require.NotContains(t, k.scripts[0], "bridge")
	k.mu.Unlock()

	guard.mu.Lock()
	require.NoError(t, probeGuard())
	guard.mu.Unlock()
	require.Equal(t, []string{"list chain inet " + guardTable + " prerouting\n"}, listed)

	st := CheckGuard()
	require.True(t, st.NoBridgeFamily)
	require.False(t, st.Enforced)

	k.mu.Lock()
	k.loaded, k.failNft = false, true
	k.mu.Unlock()
	require.NoError(t, RequireGuard(), "a guard that cannot load on WSL must not block guests")
}

// The host-access opt-out is kept by InstallGuard and applied by every
// reinstall, and reported by CheckGuard.
func TestInstallGuardAppliesHostAccess(t *testing.T) {
	k := &fakeGuardKernel{}
	withFakeGuard(t, k, "", false)
	prevWSL := guardIsWSL
	guardIsWSL = func() bool { return false }
	t.Cleanup(func() { guardIsWSL = prevWSL })

	require.NoError(t, InstallGuard("10.53.0.53", GuardOptions{}))
	k.mu.Lock()
	require.Contains(t, k.scripts[len(k.scripts)-1], `iifname "jerboa-br*" drop`)
	k.mu.Unlock()

	require.NoError(t, InstallGuard("10.53.0.53", GuardOptions{HostAccess: true}))
	k.mu.Lock()
	k.loaded = false // a firewall reload
	k.mu.Unlock()
	require.NoError(t, EnsureGuard())
	k.mu.Lock()
	last := k.scripts[len(k.scripts)-1]
	k.mu.Unlock()
	require.NotContains(t, last, `iifname "jerboa-br*" drop`)
	require.Contains(t, last, `iifname != "lo" ip daddr 127.0.0.0/8 drop`)
	require.True(t, CheckGuard().HostAccess)
}
