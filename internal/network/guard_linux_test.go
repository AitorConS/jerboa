//go:build linux

package network

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTapElementsValidates(t *testing.T) {
	if _, err := tapElements("jt-abc", "AA:FC:0A:64:00:06", "10.100.0.6"); err != nil {
		t.Fatalf("valid tap: %v", err)
	}
	for _, c := range []struct{ tap, mac, ip string }{
		{`jt-a" }`, "AA:FC:0A:64:00:06", "10.100.0.6"},
		{"jt-abc", "zz", "10.100.0.6"},
		{"jt-abc", "AA:FC:0A:64:00:06", "::1"},
		{"jt-abc", "AA:FC:0A:64:00:06", "10.0.0.1; flush ruleset"},
	} {
		if _, err := tapElements(c.tap, c.mac, c.ip); err == nil {
			t.Errorf("tapElements(%q, %q, %q) accepted invalid input", c.tap, c.mac, c.ip)
		}
	}
}

func TestTapElementsOrder(t *testing.T) {
	elems, _ := tapElements("jt-abc", "aa:fc:0a:64:00:06", "10.100.0.6")
	lines := strings.Split(strings.TrimSpace(elems), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], " taps ") {
		t.Fatalf("taps element must be added last: %q", elems)
	}
	if rev := reverseLines(elems); !strings.HasPrefix(rev, "element bridge "+guardTable+" taps") {
		t.Fatalf("taps element must be removed first: %q", rev)
	}
}

func TestCheckManagedBridgeName(t *testing.T) {
	for _, ok := range []string{"jerboa-br0", "jerboa-br-web"} {
		if err := checkManagedBridgeName(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"docker0", "eth0", "br0", "jerboa-br-toolongname", "jerboa-br/x", ""} {
		if checkManagedBridgeName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGuardRulesetMentionsDNS(t *testing.T) {
	// The address answers only on loopback and jerboa bridges, with or
	// without host access.
	for _, o := range []rulesetOptions{{}, {hostAccess: true}} {
		r := guardRuleset("10.53.0.53", o)
		for _, rule := range []string{
			`ip daddr 10.53.0.53 iifname "lo" return`,
			`ip daddr 10.53.0.53 iifname "jerboa-br*" return`,
			`ip daddr 10.53.0.53 drop`,
		} {
			if !strings.Contains(r, rule) {
				t.Fatalf("%+v: dns rule %q missing: %s", o, rule, r)
			}
		}
	}
}

// chainOf returns the body of one chain of a ruleset.
func chainOf(t *testing.T, ruleset, chain string) string {
	t.Helper()
	start := strings.Index(ruleset, "chain "+chain+" {")
	if start < 0 {
		t.Fatalf("chain %s missing: %s", chain, ruleset)
	}
	end := strings.Index(ruleset[start:], "\n\t}")
	return ruleset[start : start+end]
}

// requireOrder fails unless every rule is in body, in this order.
func requireOrder(t *testing.T, body string, rules ...string) {
	t.Helper()
	at := 0
	for _, r := range rules {
		i := strings.Index(body[at:], r)
		if i < 0 {
			t.Fatalf("rule %q missing or out of order in:\n%s", r, body)
		}
		at += i + len(r)
	}
}

// By default a guest reaches the host only for DNS and for replies to
// connections the host opened, and routes no new connection to private,
// shared or link-local addresses; what is accepted comes before the drop.
func TestGuardRulesetKeepsGuestsOffHostAndPrivateNetworks(t *testing.T) {
	r := guardRuleset("10.53.0.53", rulesetOptions{})
	requireOrder(t, chainOf(t, r, "input"),
		`iifname "jerboa-br*" ct state established,related accept`,
		`iifname "jerboa-br*" ip daddr 10.53.0.53 udp dport 53 accept`,
		`iifname "jerboa-br*" ip daddr 10.53.0.53 tcp dport 53 accept`,
		`iifname "jerboa-br*" drop`,
		`ip daddr 10.53.0.53 iifname "lo" return`,
	)
	requireOrder(t, chainOf(t, r, "forward"),
		`oifname "jerboa-br*" ct status dnat accept`,
		`oifname "jerboa-br*" ct state established,related accept`,
		`oifname "jerboa-br*" iifname != "jerboa-br*" drop`,
		`iifname "jerboa-br*" oifname != "jerboa-br*" ct state established,related accept`,
		`iifname "jerboa-br*" oifname != "jerboa-br*" ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/3 } drop`,
	)
	// Traffic between two jerboa bridges is not touched by the private
	// drop: the same network is bridged, other networks have their own rule.
	if strings.Contains(r, `iifname "jerboa-br*" ip daddr {`) {
		t.Fatalf("private drop also matches traffic to jerboa bridges: %s", r)
	}
}

// The prefixes go into one anonymous interval set, which nft refuses (and
// with it the whole guard) when two of them overlap.
func TestNonPublicDestinationsDoNotOverlap(t *testing.T) {
	var nets []*net.IPNet
	for _, c := range strings.Split(nonPublicDestinations, ", ") {
		_, n, err := net.ParseCIDR(c)
		if err != nil || n.String() != c {
			t.Fatalf("%q is not a canonical IPv4 prefix: %v", c, err)
		}
		for _, o := range nets {
			if o.Contains(n.IP) || n.Contains(o.IP) {
				t.Fatalf("%s overlaps %s", n, o)
			}
		}
		nets = append(nets, n)
	}
}

// JERBOA_GUEST_HOST_ACCESS=1 lifts the host and private-network drops only:
// the loopback drop and the isolation between networks stay.
func TestGuardRulesetWithHostAccess(t *testing.T) {
	r := guardRuleset("10.53.0.53", rulesetOptions{hostAccess: true})
	for _, gone := range []string{`iifname "jerboa-br*" drop`, `ip daddr { `, "dport 53 accept"} {
		if strings.Contains(r, gone) {
			t.Fatalf("host access still has %q: %s", gone, r)
		}
	}
	for _, kept := range []string{
		`iifname != "lo" ip daddr 127.0.0.0/8 drop`,
		`oifname "jerboa-br*" iifname != "jerboa-br*" drop`,
		`ether type != { ip, arp } drop`,
	} {
		if !strings.Contains(r, kept) {
			t.Fatalf("host access lost %q: %s", kept, r)
		}
	}
}

func TestGuardRulesetDropsSpoofedLoopbackAndNonIPv4Frames(t *testing.T) {
	r := guardRuleset("10.53.0.53", rulesetOptions{})
	// Loopback destinations only come in on lo: not from a guest's bridge
	// and not from another host.
	if !strings.Contains(r, "type filter hook prerouting priority raw; policy accept;\n\t\tiifname != \"lo\" ip daddr 127.0.0.0/8 drop") {
		t.Fatalf("loopback guard missing: %s", r)
	}
	// Guarded taps send only IPv4 and ARP, after the MAC check.
	mac := strings.Index(r, "iifname . ether saddr != @tap_mac drop")
	types := strings.Index(r, "ether type != { ip, arp } drop")
	if mac < 0 || types < mac {
		t.Fatalf("ether type filter missing or before the MAC check: %s", r)
	}
}

// WSL kernels have no nftables bridge family: a ruleset holding the bridge
// table would fail as a whole there.
func TestGuardRulesetWithoutBridgeFamily(t *testing.T) {
	r := guardRuleset("10.53.0.53", rulesetOptions{noBridge: true})
	if strings.Contains(r, "table bridge") || strings.Contains(r, "ether ") {
		t.Fatalf("bridge table in a ruleset for a kernel without the bridge family: %s", r)
	}
	if !strings.Contains(r, `iifname != "lo" ip daddr 127.0.0.0/8 drop`) {
		t.Fatalf("loopback guard missing: %s", r)
	}
}

func TestDisableIPv6(t *testing.T) {
	dir := t.TempDir()
	prev := ipv6ConfDir
	ipv6ConfDir = dir
	t.Cleanup(func() { ipv6ConfDir = prev })

	// A kernel without IPv6 has nothing to change.
	if err := disableIPv6("jerboa-br-x"); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "jerboa-br-x")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := disableIPv6("jerboa-br-x"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"accept_ra": "0", "disable_ipv6": "1"} {
		got, err := os.ReadFile(filepath.Join(conf, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
}
