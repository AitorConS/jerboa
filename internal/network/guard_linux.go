//go:build linux

package network

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Guard rules for guest networks, in nftables tables owned by jerboad and
// independent of the NAT table and of other firewall rules:
//
//   - guests cannot reach the host's loopback through their bridge (the
//     bridges have route_localnet set for kernel port publishing);
//   - the guest DNS address answers only on loopback and jerboa bridges;
//   - traffic is not routed between two jerboa networks, and hosts outside
//     only reach guests through published ports or replies;
//   - a guest can only send frames with its own MAC and IPv4 address.
//
// Installing the guard is best-effort: without nft the daemon logs a warning
// and runs without it. IPv4 forwarding stays enabled host-wide, as routing
// guest traffic needs it.
const guardTable = "jerboa_guard"

var guard struct {
	mu        sync.Mutex
	installed bool
	bridges   map[string]bool
}

func guardRuleset(dnsIP string) string {
	return `table inet ` + guardTable + ` {
	chain prerouting {
		type filter hook prerouting priority raw; policy accept;
		iifname "jerboa-br*" ip daddr 127.0.0.0/8 drop
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
	if _, err := exec.LookPath("nft"); err != nil {
		return errors.New("guard: nft not found")
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	script := "table inet " + guardTable + "\ndelete table inet " + guardTable + "\n" +
		"add table bridge " + guardTable + "\n" +
		"add chain bridge " + guardTable + " prerouting\n" +
		"flush chain bridge " + guardTable + " prerouting\n" +
		"delete chain bridge " + guardTable + " prerouting\n" +
		guardRuleset(parsed.String())
	bridges := existingBridges()
	for _, b := range bridges {
		script += bridgeIsolationRule(b)
	}
	if err := runNft(script); err != nil {
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
	if err := runNft(bridgeIsolationRule(bridge)); err != nil {
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
	if !guard.installed {
		return nil
	}
	// Drop bindings a previous guest left on a reused tap name first. The
	// taps element goes last, so the tap is never listed without its
	// bindings.
	for _, l := range strings.SplitAfter(elems, "\n") {
		if l != "" {
			_ = runNft("delete " + l)
		}
	}
	if err := runNft(prefixLines(elems, "add ")); err != nil {
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
	if !guard.installed {
		return
	}
	if err := runNft(prefixLines(reverseLines(elems), "delete ")); err != nil {
		slog.Debug("guard: unguard tap", "tap", tap, "err", err)
	}
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
