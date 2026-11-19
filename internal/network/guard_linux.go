//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// Guard rules for guest networks, in nftables tables owned by jerboad and
// independent of the NAT table and of other firewall rules:
//
//   - nothing reaches the host's loopback addresses from outside the host:
//     the bridges have route_localnet set for kernel port publishing, and a
//     packet to 127.0.0.0/8 that arrives on another interface is spoofed;
//   - the guest DNS address answers only on loopback and jerboa bridges;
//   - a guest reaches the host only for DNS and for replies to connections
//     the host opened (published ports, health checks), and opens no
//     connection to private, shared (CGNAT) or link-local addresses outside
//     its own network: the same default as macOS, lifted by
//     JERBOA_GUEST_HOST_ACCESS=1;
//   - traffic is not routed between two jerboa networks, and hosts outside
//     only reach guests through published ports or replies;
//   - a guest can only send IPv4 and ARP frames, with its own MAC and IPv4
//     address: IPv6 (router advertisements included) and VLAN-tagged frames
//     are dropped, as on macOS.
//
// Guests join a network only while the guard is loaded (RequireGuard): without
// nft, or with a ruleset the kernel refuses, a VM on a network does not start,
// unless the daemon runs with JERBOA_ALLOW_UNGUARDED_NETWORK=1. On WSL the
// kernel has no nftables bridge family: only the inet table is installed, so
// guests are not bound to their MAC and IPv4 address there, and a guard that
// cannot be loaded is not enforced. IPv4 forwarding stays enabled host-wide,
// as routing guest traffic needs it.
//
// The tables can disappear behind the daemon: "flush ruleset", the first line
// of the stock /etc/nftables.conf, removes them on every start or reload of
// nftables.service. WatchGuard checks the kernel and reinstalls them, and a
// bridge only gets route_localnet while the loopback drop is verifiably
// loaded (see EnsureGuard and nftNAT.prepareBridge).
const guardTable = "jerboa_guard"

var guard struct {
	mu        sync.Mutex
	installed bool
	// dnsIP is the guest DNS address InstallGuard was given, kept so the
	// guard can be reinstalled; empty until InstallGuard is called.
	dnsIP   string
	bridges map[string]bool
	// allowUnguarded lets guests join networks while the guard is not
	// loaded (GuardOptions.AllowUnguarded).
	allowUnguarded bool
	// noBridge is set where the kernel has no nftables bridge family
	// (WSL): the bridge table and the tap bindings are left out.
	noBridge bool
	// hostAccess leaves out the rules that keep guests off the host and
	// off private networks (GuardOptions.HostAccess).
	hostAccess bool
	// taps holds the set elements GuardTap bound to each tap, so a
	// reinstall restores the bindings of the guests that are running.
	taps map[string]string
}

// Test seams: the nft calls of the guard, the check that it is loaded, and
// what the host offers.
var (
	guardNft    = runNft
	guardList   = nftOutput
	guardProbe  = probeGuard
	guardHasNft = func() bool { return hasTool("nft") }
	guardIsWSL  = isWSL
)

// errNoNft is why the guard cannot be installed on a host without nft.
var errNoNft = errors.New("guard: nft not found (the nftables package is not installed)")

// probeGuard reports whether the loopback drop and the bridge table of the
// guard are loaded, asking the kernel rather than trusting guard.installed.
// guard.mu must be held.
func probeGuard() error {
	// One list command per run: nft 1.0.9 fails a script holding two.
	if !guard.noBridge {
		if _, err := guardList("list table bridge " + guardTable + "\n"); err != nil {
			return fmt.Errorf("guard: tables not loaded: %w", err)
		}
	}
	out, err := guardList("list chain inet " + guardTable + " prerouting\n")
	if err != nil {
		return fmt.Errorf("guard: tables not loaded: %w", err)
	}
	if !strings.Contains(out, "ip daddr 127.0.0.0/8 drop") {
		return errors.New("guard: loopback drop rule missing")
	}
	return nil
}

// rulesetOptions are the host properties guardRuleset depends on.
type rulesetOptions struct {
	// noBridge leaves the bridge table out (no nftables bridge family).
	noBridge bool
	// hostAccess leaves out the drops of new connections from guests to the
	// host and to private networks.
	hostAccess bool
}

// nonPublicDestinations are the addresses a guest opens no connection to
// outside its own network: the list macOS applies (nonPublicEgress in
// internal/vm). Elements must not overlap: nft refuses an anonymous interval
// set whose elements do, and with it the whole guard.
const nonPublicDestinations = "0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/3"

// guestInputRules keep guests off the host's services: replies to
// connections the host opened (published ports through the proxy or kernel
// NAT, health checks) and guest DNS are accepted, anything else a guest
// starts towards the host is dropped.
func guestInputRules(dnsIP string, o rulesetOptions) string {
	if o.hostAccess {
		return ""
	}
	return `
		iifname "jerboa-br*" ct state established,related accept
		iifname "jerboa-br*" ip daddr ` + dnsIP + ` udp dport 53 accept
		iifname "jerboa-br*" ip daddr ` + dnsIP + ` tcp dport 53 accept
		iifname "jerboa-br*" drop`
}

// guestForwardRules drop new connections a guest routes to non-public
// addresses. Traffic that stays on jerboa bridges is left to the rules
// above and below (the same network is bridged, not routed; other networks
// are dropped by bridgeIsolationRule), and replies (published ports with
// nat-lan, whose clients are often on private addresses) are accepted.
func guestForwardRules(o rulesetOptions) string {
	if o.hostAccess {
		return ""
	}
	return `
		iifname "jerboa-br*" oifname != "jerboa-br*" ct state established,related accept
		iifname "jerboa-br*" oifname != "jerboa-br*" ip daddr { ` + nonPublicDestinations + ` } drop`
}

func guardRuleset(dnsIP string, o rulesetOptions) string {
	inet := `table inet ` + guardTable + ` {
	chain prerouting {
		type filter hook prerouting priority raw; policy accept;
		iifname != "lo" ip daddr 127.0.0.0/8 drop
	}
	chain input {
		type filter hook input priority filter; policy accept;` + guestInputRules(dnsIP, o) + `
		ip daddr ` + dnsIP + ` iifname "lo" return
		ip daddr ` + dnsIP + ` iifname "jerboa-br*" return
		ip daddr ` + dnsIP + ` drop
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		oifname "jerboa-br*" ct status dnat accept
		oifname "jerboa-br*" ct state established,related accept
		oifname "jerboa-br*" iifname != "jerboa-br*" drop` + guestForwardRules(o) + `
	}
}
`
	if o.noBridge {
		return inet
	}
	return inet + `table bridge ` + guardTable + ` {
	set taps {
		type ifname
	}
	set tap_mac {
		type ifname . ether_addr
	}
	set tap_ip {
		type ifname . ipv4_addr
	}
	chain prerouting {
		type filter hook prerouting priority filter; policy accept;
		iifname != @taps return
		iifname . ether saddr != @tap_mac drop
		ether type != { ip, arp } drop
		ether type ip iifname . ip saddr != @tap_ip drop
		ether type arp iifname . arp saddr ip != @tap_ip drop
	}
}
`
}

// bridgeIsolationRule drops traffic routed from bridge to another jerboa
// bridge.
func bridgeIsolationRule(bridge string) string {
	return `add rule inet ` + guardTable + ` forward iifname "` + bridge + `" oifname "jerboa-br*" oifname != "` + bridge + `" drop` + "\n"
}

// InstallGuard (re)creates the guard tables. Tap elements are kept across
// daemon restarts, so guests that kept running stay guarded; the routing
// rules are rebuilt for the jerboa bridges that exist. The configuration is
// kept when it fails, so RequireGuard and WatchGuard can install it later.
func InstallGuard(dnsIP string, opts GuardOptions) error {
	parsed := net.ParseIP(dnsIP)
	if parsed == nil || parsed.To4() == nil {
		return fmt.Errorf("guard: %q is not a valid IPv4 address", dnsIP)
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	guard.dnsIP = parsed.String()
	guard.allowUnguarded = opts.AllowUnguarded
	guard.hostAccess = opts.HostAccess
	guard.noBridge = guardIsWSL()
	if !guardHasNft() {
		return errNoNft
	}
	return installGuardLocked(guard.dnsIP)
}

// installGuardLocked installs the guard tables for dnsIP and restores the tap
// bindings GuardTap recorded. guard.mu must be held.
func installGuardLocked(dnsIP string) error {
	script := "table inet " + guardTable + "\ndelete table inet " + guardTable + "\n"
	if !guard.noBridge {
		script += "add table bridge " + guardTable + "\n" +
			"add chain bridge " + guardTable + " prerouting\n" +
			"flush chain bridge " + guardTable + " prerouting\n" +
			"delete chain bridge " + guardTable + " prerouting\n"
	}
	script += guardRuleset(dnsIP, rulesetOptions{noBridge: guard.noBridge, hostAccess: guard.hostAccess})
	bridges := existingBridges()
	for _, b := range bridges {
		script += bridgeIsolationRule(b)
	}
	// Bindings of running guests: the bridge table keeps them across a
	// daemon restart, but not across a flush of the whole ruleset.
	if !guard.noBridge {
		for _, elems := range guard.taps {
			script += prefixLines(elems, "add ")
		}
	}
	if err := guardNft(script); err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	guard.installed = true
	guard.bridges = make(map[string]bool, len(bridges))
	for _, b := range bridges {
		guard.bridges[b] = true
	}
	return nil
}

// existingBridges lists the jerboa bridges present on the host.
func existingBridges() []string {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if checkManagedBridgeName(n) == nil && isBridge(n) {
			out = append(out, n)
		}
	}
	return out
}

// GuardBridge isolates bridge from the other jerboa networks. It is a no-op
// when the guard is not installed.
func GuardBridge(bridge string) error {
	if err := checkManagedBridgeName(bridge); err != nil {
		return err
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if !guard.installed || guard.bridges[bridge] {
		return nil
	}
	if err := guardNft(bridgeIsolationRule(bridge)); err != nil {
		return fmt.Errorf("guard bridge %s: %w", bridge, err)
	}
	guard.bridges[bridge] = true
	return nil
}

// tapElements returns the set elements binding tap to mac and ip, the taps
// element last.
func tapElements(tap, mac, ip string) (string, error) {
	if !validIfaceName(tap) {
		return "", fmt.Errorf("invalid tap name %q", tap)
	}
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return "", fmt.Errorf("invalid guest mac %q", mac)
	}
	addr := net.ParseIP(ip)
	if addr == nil || addr.To4() == nil {
		return "", fmt.Errorf("invalid guest ip %q", ip)
	}
	t := `"` + tap + `"`
	return "element bridge " + guardTable + " tap_mac { " + t + " . " + hw.String() + " }\n" +
		"element bridge " + guardTable + " tap_ip { " + t + " . " + addr.To4().String() + " }\n" +
		"element bridge " + guardTable + " taps { " + t + " }\n", nil
}

// GuardTap restricts the frames a guest sends through tap to its own MAC and
// IPv4 address. It is a no-op when the guard is not installed or the kernel
// has no nftables bridge family (WSL).
func GuardTap(tap, mac, ip string) error {
	elems, err := tapElements(tap, mac, ip)
	if err != nil {
		return err
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	// Remembered even while the guard is not installed, so installing it
	// later binds this guest too.
	if guard.taps == nil {
		guard.taps = make(map[string]string)
	}
	guard.taps[tap] = elems
	if !guard.installed || guard.noBridge {
		return nil
	}
	// Drop bindings a previous guest left on a reused tap name first. The
	// taps element goes last, so the tap is never listed without its
	// bindings.
	for _, l := range strings.SplitAfter(elems, "\n") {
		if l != "" {
			_ = guardNft("delete " + l)
		}
	}
	if err := guardNft(prefixLines(elems, "add ")); err != nil {
		delete(guard.taps, tap)
		return fmt.Errorf("guard tap %s: %w", tap, err)
	}
	return nil
}

// UnguardTap removes the bindings added by GuardTap.
func UnguardTap(tap, mac, ip string) {
	elems, err := tapElements(tap, mac, ip)
	if err != nil {
		return
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.taps[tap] == elems {
		delete(guard.taps, tap)
	}
	if !guard.installed || guard.noBridge {
		return
	}
	if err := guardNft(prefixLines(reverseLines(elems), "delete ")); err != nil {
		slog.Debug("guard: unguard tap", "tap", tap, "err", err)
	}
}

// EnsureGuard reports whether the guard is loaded in the kernel, and
// reinstalls it (tap bindings included) when its tables were removed behind
// the daemon or could not be installed at start. It fails when InstallGuard
// was never called or the guard cannot be loaded.
func EnsureGuard() error {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return ensureGuardLocked()
}

// ensureGuardLocked is EnsureGuard with guard.mu held.
func ensureGuardLocked() error {
	if guard.dnsIP == "" {
		return errors.New("guard: not configured")
	}
	err := guardProbe()
	if err == nil {
		return nil
	}
	if guard.installed {
		slog.Warn("guard: tables were removed from the kernel; reinstalling", "err", err)
	}
	guard.installed = false
	if err := installGuardLocked(guard.dnsIP); err != nil {
		return err
	}
	if err := guardProbe(); err != nil {
		guard.installed = false
		return err
	}
	slog.Info("guard: installed")
	return nil
}

// loadGuardLocked loads the guard if it is not, and says why it cannot be.
// guard.mu must be held.
func loadGuardLocked() error {
	if !guardHasNft() {
		return errNoNft
	}
	return ensureGuardLocked()
}

// RequireGuard is called before a guest is attached to a bridge. It loads the
// guard when it is not (nftables installed since the daemon started, or a
// firewall reload that removed it), and fails when it cannot be loaded: a
// guest on an unguarded bridge could spoof other guests and reach the host's
// services. JERBOA_ALLOW_UNGUARDED_NETWORK=1 lets the guest start anyway, and
// on WSL the guard is not enforced (see the comment at the top).
func RequireGuard() error {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	err := loadGuardLocked()
	switch {
	case err == nil:
		return nil
	case guard.allowUnguarded:
		slog.Warn("guard: not loaded; the guest joins its network without isolation because "+AllowUnguardedEnv+"=1", "err", err)
		return nil
	case guard.noBridge:
		slog.Warn("guard: not loaded on WSL; the guest joins its network without isolation", "err", err)
		return nil
	}
	return fmt.Errorf("guest network isolation is not available, so VMs on networks do not start: %w. "+
		"Install nftables (sudo apt-get install nftables) and run the VM again; "+
		"to run guests without isolation instead, start jerboad with %s=1", err, AllowUnguardedEnv)
}

// CheckGuard loads the guard if it can and reports its state.
func CheckGuard() GuardState {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	st := GuardState{
		Configured:     guard.dnsIP != "",
		AllowUnguarded: guard.allowUnguarded,
		NoBridgeFamily: guard.noBridge,
		HostAccess:     guard.hostAccess,
		Enforced:       !guard.allowUnguarded && !guard.noBridge,
	}
	if st.Configured {
		st.Err = loadGuardLocked()
	}
	return st
}

// GuardCheckInterval is how often jerboad checks that the guard is loaded.
const GuardCheckInterval = 5 * time.Second

// WatchGuard checks the guard every interval until ctx ends. It reinstalls
// tables that were removed and, while they cannot be loaded, turns
// route_localnet off on the bridges kernel NAT turned it on for, so no guest
// reaches the host's loopback; route_localnet comes back with the guard.
func WatchGuard(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			checkGuard()
		}
	}
}

// checkGuard is one round of WatchGuard.
func checkGuard() {
	if err := loopbackGuard(); err != nil {
		if disableRouteLocalnet() {
			slog.Error("guard: not loaded; route_localnet turned off on jerboa bridges, so host clients on 127.0.0.1 cannot reach kernel-NAT published ports until it is back", "err", err)
		}
		return
	}
	restoreRouteLocalnet()
}

func prefixLines(s, prefix string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		if l != "" {
			b.WriteString(prefix + l)
		}
	}
	return b.String()
}

func reverseLines(s string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

func validIfaceName(n string) bool {
	if n == "" || len(n) > maxIfaceNameLen {
		return false
	}
	for _, c := range n {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
