//go:build linux

package network

import (
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
