//go:build linux

package network

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNATScript(t *testing.T) {
	entries := []natEntry{
		{proto: "tcp", hostPort: 6379, guest: net.IPv4(10, 100, 0, 2).To4(), guestPort: 6379},
		{bind: net.IPv4(127, 0, 0, 1).To4(), proto: "udp", hostPort: 5353, guest: net.IPv4(10, 100, 0, 2).To4(), guestPort: 53},
	}
	require.Equal(t,
		"add element ip jerboa pub_any { tcp . 6379 : 10.100.0.2 . 6379 }\n"+
			"add element ip jerboa pub_addr { 127.0.0.1 . udp . 5353 : 10.100.0.2 . 53 }\n",
		natScript("add", entries))
	require.Equal(t,
		"delete element ip jerboa pub_any { tcp . 6379 }\n"+
			"delete element ip jerboa pub_addr { 127.0.0.1 . udp . 5353 }\n",
		natScript("delete", entries))
}

func TestPrepareBridgeRejectsPaths(t *testing.T) {
	require.Error(t, nftNAT{}.prepareBridge("../all"))
	require.Error(t, nftNAT{}.prepareBridge(""))
}

func TestNATRulesetLAN(t *testing.T) {
	local := natRuleset(false)
	require.NotContains(t, local, "prerouting")
	require.Contains(t, local, "hook output")

	lan := natRuleset(true)
	require.Contains(t, lan, "type nat hook prerouting priority dstnat;")
	// Guests keep using the proxy, and only local destinations are translated.
	require.Contains(t, lan, "iifname \"jerboa-br*\" return\n\t\tfib daddr type local dnat ip to meta l4proto . th dport map @pub_any")
	require.Contains(t, lan, "dnat ip to ip daddr . meta l4proto . th dport map @pub_addr")
	require.Contains(t, lan, "hook output")
}

func TestForwardRulesOnlyAcceptTranslatedGuestTraffic(t *testing.T) {
	rules := forwardRules()
	require.Len(t, rules, 2)
	for i, dir := range []string{"-o", "-i"} {
		require.Equal(t, []string{dir, "jerboa-br+", "-m", "conntrack", "--ctstate", "DNAT",
			"-m", "comment", "--comment", forwardComment, "-j", "ACCEPT"}, rules[i])
	}
}

// A ruleset with iptables-nft (Docker), firewalld, nftables.service, an IPv6
// table, jerboa's own table, and two rules jerboad left in firewalld.
const sampleNftRuleset = `{"nftables": [
 {"metainfo": {"version": "1.1.1", "json_schema_version": 1}},
 {"chain": {"family": "ip", "table": "filter", "name": "FORWARD", "handle": 2, "type": "filter", "hook": "forward", "prio": 0, "policy": "drop"}},
 {"chain": {"family": "ip", "table": "filter", "name": "DOCKER-USER", "handle": 9}},
 {"chain": {"family": "inet", "table": "firewalld", "name": "filter_FORWARD", "handle": 175, "type": "filter", "hook": "forward", "prio": 10, "policy": "accept"}},
 {"chain": {"family": "inet", "table": "filter", "name": "forward", "handle": 2, "type": "filter", "hook": "forward", "prio": 0, "policy": "drop"}},
 {"chain": {"family": "ip6", "table": "filter6", "name": "forward", "handle": 1, "type": "filter", "hook": "forward", "prio": 0, "policy": "drop"}},
 {"chain": {"family": "ip", "table": "jerboa", "name": "prerouting", "handle": 4, "type": "nat", "hook": "prerouting", "prio": -100, "policy": "accept"}},
 {"rule": {"family": "inet", "table": "firewalld", "chain": "filter_FORWARD", "handle": 301, "comment": "jerboa published ports", "expr": []}},
 {"rule": {"family": "inet", "table": "firewalld", "chain": "filter_FORWARD", "handle": 302, "comment": "jerboa published ports", "expr": []}},
 {"rule": {"family": "inet", "table": "filter", "chain": "forward", "handle": 7, "comment": "user rule", "expr": []}}
]}`

func TestForeignForwardChains(t *testing.T) {
	objs, err := parseNftRuleset([]byte(sampleNftRuleset))
	require.NoError(t, err)
	// iptables-nft's FORWARD takes the iptables rules; IPv6 and non-forward
	// chains are left alone.
	require.Equal(t, []nftChain{
		{"inet", "firewalld", "filter_FORWARD"},
		{"inet", "filter", "forward"},
	}, foreignForwardChains(objs))
}

func TestOwnedNftRules(t *testing.T) {
	objs, err := parseNftRuleset([]byte(sampleNftRuleset))
	require.NoError(t, err)
	fw := nftChain{"inet", "firewalld", "filter_FORWARD"}
	require.Equal(t, []nftOwnedRule{{fw, 301}, {fw, 302}}, ownedNftRules(objs))

	_, err = parseNftRuleset([]byte("not json"))
	require.Error(t, err)
}

func TestNftForwardRulesMatchIptablesOnes(t *testing.T) {
	rules := nftForwardRules()
	require.Len(t, rules, len(forwardRules()))
	for i, dir := range []string{"oifname", "iifname"} {
		require.Equal(t, dir+` "jerboa-br*" ct status dnat accept comment "jerboa published ports"`, rules[i])
	}
	require.Equal(t, `"filter_FORWARD"`, nftQuote("filter_FORWARD"))
}
