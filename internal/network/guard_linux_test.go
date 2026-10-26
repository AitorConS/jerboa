//go:build linux

package network

import (
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
	r := guardRuleset("10.53.0.53")
	if strings.Count(r, "10.53.0.53") != 3 {
		t.Fatalf("dns address not used in input chain: %s", r)
	}
}

func TestGuardRulesetDropsSpoofedLoopbackAndNonIPv4Frames(t *testing.T) {
	r := guardRuleset("10.53.0.53")
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
