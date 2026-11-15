//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
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
//   - traffic is not routed between two jerboa networks, and hosts outside
//     only reach guests through published ports or replies;
//   - a guest can only send IPv4 and ARP frames, with its own MAC and IPv4
//     address: IPv6 (router advertisements included) and VLAN-tagged frames
//     are dropped, as on macOS.
//
// Installing the guard is best-effort: without nft the daemon logs a warning
// and runs without it. IPv4 forwarding stays enabled host-wide, as routing
// guest traffic needs it.
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
	// taps holds the set elements GuardTap bound to each tap, so a
	// reinstall restores the bindings of the guests that are running.
	taps map[string]string
}

// Test seams: the nft calls of the guard, and the check that it is loaded.
var (
	guardNft   = runNft
	guardProbe = probeGuard
)

// probeGuard reports whether the loopback drop and the bridge table of the
// guard are loaded, asking the kernel rather than trusting guard.installed.
func probeGuard() error {
	// One list command per run: nft 1.0.9 fails a script holding two.
	if _, err := nftOutput("list table bridge " + guardTable + "\n"); err != nil {
		return fmt.Errorf("guard: tables not loaded: %w", err)
	}
	out, err := nftOutput("list chain inet " + guardTable + " prerouting\n")
	if err != nil {
		return fmt.Errorf("guard: tables not loaded: %w", err)
	}
	if !strings.Contains(out, "ip daddr 127.0.0.0/8 drop") {
		return errors.New("guard: loopback drop rule missing")
	}
	return nil
}

func guardRuleset(dnsIP string) string {
	return `table inet ` + guardTable + ` {
	chain prerouting {
		type filter hook prerouting priority raw; policy accept;
		iifname != "lo" ip daddr 127.0.0.0/8 drop
	}
	chain input {
		type filter hook input priority filter; policy accept;
		ip daddr ` + dnsIP + ` iifname "lo" return
		ip daddr ` + dnsIP + ` iifname "jerboa-br*" return
		ip daddr ` + dnsIP + ` drop
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		oifname "jerboa-br*" ct status dnat accept
		oifname "jerboa-br*" ct state established,related accept
		oifname "jerboa-br*" iifname != "jerboa-br*" drop
	}
}
table bridge ` + guardTable + ` {
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
// rules are rebuilt for the jerboa bridges that exist.
func InstallGuard(dnsIP string) error {
	parsed := net.ParseIP(dnsIP)
	if parsed == nil || parsed.To4() == nil {
		return fmt.Errorf("guard: %q is not a valid IPv4 address", dnsIP)
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	guard.dnsIP = parsed.String()
	if _, err := exec.LookPath("nft"); err != nil {
		return errors.New("guard: nft not found")
	}
	return installGuardLocked(guard.dnsIP)
}

// installGuardLocked installs the guard tables for dnsIP and restores the tap
// bindings GuardTap recorded. guard.mu must be held.
func installGuardLocked(dnsIP string) error {
	script := "table inet " + guardTable + "\ndelete table inet " + guardTable + "\n" +
		"add table bridge " + guardTable + "\n" +
		"add chain bridge " + guardTable + " prerouting\n" +
		"flush chain bridge " + guardTable + " prerouting\n" +
		"delete chain bridge " + guardTable + " prerouting\n" +
		guardRuleset(dnsIP)
	bridges := existingBridges()
	for _, b := range bridges {
		script += bridgeIsolationRule(b)
	}
	// Bindings of running guests: the bridge table keeps them across a
	// daemon restart, but not across a flush of the whole ruleset.
	for _, elems := range guard.taps {
		script += prefixLines(elems, "add ")
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
// IPv4 address. It is a no-op when the guard is not installed.
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
	if !guard.installed {
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
	if !guard.installed {
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
