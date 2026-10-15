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
