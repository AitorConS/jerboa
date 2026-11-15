//go:build linux

package network

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Linux backend reads tables the kernel and other tools print
// (/proc/net/route, `nft -j list ruleset`) and writes firewall scripts from
// names and addresses that came in through the API. A table cut short or
// reshaped must not crash the daemon or invent a conflict, and nothing but
// validated values may reach a script.

func cidr(t testing.TB, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	require.NoError(t, err)
	return n
}

func TestRouteConflict_TableCutAtEveryByte(t *testing.T) {
	want := cidr(t, "192.168.0.0/16")
	// The route file is read while the kernel changes it; a read can end
	// anywhere. Whatever is there, the answer names a real route or nothing.
	for n := range len(procRoute) + 1 {
		got := routeConflict(strings.NewReader(procRoute[:n]), want)
		if got != "" {
			require.Equal(t, "route 192.168.1.0/24 on eth0", got, "table cut at byte %d", n)
		}
	}
	// A row cut inside its mask column is not a route with a shorter mask.
	rowEnd := strings.Index(procRoute, "00FFFFFF") + len("00FFFF")
	require.Empty(t, routeConflict(strings.NewReader(procRoute[:rowEnd]), want))
}

func TestRouteConflict_MalformedRows(t *testing.T) {
	header := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n"
	everything := cidr(t, "10.0.0.0/8")
	for name, rows := range map[string]string{
		"empty file":          "",
		"header only":         header,
		"blank lines":         header + "\n\n\n",
		"too few columns":     header + "eth0\t0000000A\t00000000\t0001\n",
		"not hex":             header + "eth0\tZZZZZZZZ\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n",
		"short hex":           header + "eth0\t00000A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n",
		"long hex":            header + "eth0\t0000000A00\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n",
		"mask not hex":        header + "eth0\t0000000A\t00000000\t0001\t0\t0\t0\tgarbage!\t0\t0\t0\n",
		"mask with a gap":     header + "eth0\t0000000A\t00000000\t0001\t0\t0\t0\t00FF00FF\t0\t0\t0\n",
		"default route":       header + "eth0\t00000000\t0101000A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n",
		"own bridge":          header + "jerboa-br0\t0000000A\t00000000\t0001\t0\t0\t0\t000000FF\t0\t0\t0\n",
		"own network bridge":  header + "jerboa-br-app\t0000000A\t00000000\t0001\t0\t0\t0\t000000FF\t0\t0\t0\n",
		"binary":              header + "\x00\x01\x02\xff\xfe\n",
		"nul separated":       header + "eth0\x000000000A\x0000000000\x000001\x000\x000\x000\x0000FFFFFF\n",
		"route out of range":  header + "eth0\t0000A8C0\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n",
		"only the first line": "eth0\t0000000A\t00000000\t0001\t0\t0\t0\t000000FF\t0\t0\t0\n",
	} {
		assert.Empty(t, routeConflict(strings.NewReader(rows), everything), name)
	}

	// Rows that cannot be read do not hide the ones that can.
	mixed := header + "garbage\n\neth0\tZZ\n" + "wg0\t0000640A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	assert.Equal(t, "route 10.100.0.0/16 on wg0", routeConflict(strings.NewReader(mixed), cidr(t, "10.100.7.0/24")))
}

// errReader fails after its content, as a /proc read interrupted mid-way.
type errReader struct {
	r   *strings.Reader
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.r.Len() == 0 {
		return 0, e.err
	}
	return e.r.Read(p)
}

func TestRouteConflict_ReadError(t *testing.T) {
	// The read fails after the first routes: the routes already seen still
	// count, and the failure itself is not a panic.
	r := &errReader{r: strings.NewReader(procRoute), err: assert.AnError}
	assert.Equal(t, "route 172.17.0.0/16 on docker0", routeConflict(r, cidr(t, "172.17.5.0/24")))
	r = &errReader{r: strings.NewReader(""), err: assert.AnError}
	assert.Empty(t, routeConflict(r, cidr(t, "172.17.5.0/24")))
}

func FuzzRouteConflict(f *testing.F) {
	f.Add([]byte(procRoute), "192.168.1.0/24")
	f.Add([]byte(procRoute[:len(procRoute)/2]), "172.17.0.0/16")
	f.Add([]byte("h\neth0 0000000A 0 0 0 0 0 00FFFFFF\n"), "10.0.0.0/24")
	f.Add([]byte("h\neth0 FFFFFFFF 0 0 0 0 0 FFFFFFFF\n"), "10.0.0.0/8")
	f.Add([]byte{0, 1, 2, '\n', 0xff}, "10.0.0.0/16")
	f.Fuzz(func(t *testing.T, table []byte, subnet string) {
		_, n, err := net.ParseCIDR(subnet)
		if err != nil || n.IP.To4() == nil {
			return
		}
		got := routeConflict(bytes.NewReader(table), n)
		if got != "" && !strings.HasPrefix(got, "route ") {
			t.Fatalf("unexpected answer %q", got)
		}
		// A table that only lists Jerboa's own bridges never conflicts.
		var own bytes.Buffer
		for i, line := range bytes.Split(table, []byte("\n")) {
			if i > 0 {
				own.WriteString(managedBridgePrefix)
			}
			own.Write(line)
			own.WriteByte('\n')
		}
		if got := routeConflict(&own, n); got != "" {
			t.Fatalf("a route on a managed bridge was reported: %q", got)
		}
	})
}

func TestParseNftRuleset_CutAtEveryByte(t *testing.T) {
	// nft was killed, or its pipe closed, part-way through its answer: that
	// is an error, never a shorter ruleset taken for the whole one (rules
	// jerboad owns would be forgotten and left in the firewall).
	for n := range len(sampleNftRuleset) {
		_, err := parseNftRuleset([]byte(sampleNftRuleset[:n]))
		require.Error(t, err, "ruleset cut at byte %d", n)
	}
}

func TestParseNftRuleset_UnexpectedShapes(t *testing.T) {
	for name, doc := range map[string]string{
		"null":               `null`,
		"empty object":       `{}`,
		"nftables null":      `{"nftables": null}`,
		"nftables empty":     `{"nftables": []}`,
		"null elements":      `{"nftables": [null, {}, {"chain": null}, {"rule": null}]}`,
		"unknown objects":    `{"nftables": [{"table": {"family": "ip", "name": "x"}}, {"set": {}}, {"flowtable": 1}]}`,
		"empty chain":        `{"nftables": [{"chain": {}}, {"rule": {}}]}`,
		"forward, no names":  `{"nftables": [{"chain": {"family": "inet", "hook": "forward"}}]}`,
		"hostile table name": `{"nftables": [{"chain": {"family": "inet", "table": "t; flush ruleset", "name": "forward", "hook": "forward"}}]}`,
		"hostile chain name": `{"nftables": [{"chain": {"family": "inet", "table": "filter", "name": "f\n delete table inet filter", "hook": "forward"}}]}`,
		"quoted name":        `{"nftables": [{"chain": {"family": "ip", "table": "\"filter\"", "name": "fwd", "hook": "forward"}}]}`,
		"non-ascii name":     `{"nftables": [{"chain": {"family": "ip", "table": "fíltér", "name": "fwd", "hook": "forward"}}]}`,
		"digit first":        `{"nftables": [{"chain": {"family": "ip", "table": "0filter", "name": "fwd", "hook": "forward"}}]}`,
		"other family":       `{"nftables": [{"chain": {"family": "bridge", "table": "filter", "name": "fwd", "hook": "forward"}}, {"chain": {"family": "ip6", "table": "filter", "name": "fwd", "hook": "forward"}}]}`,
		"other hook":         `{"nftables": [{"chain": {"family": "inet", "table": "filter", "name": "in", "hook": "input"}}]}`,
		"someone's comment":  `{"nftables": [{"rule": {"family": "inet", "table": "filter", "chain": "fwd", "handle": 4, "comment": "jerboa published ports "}}]}`,
	} {
		objs, err := parseNftRuleset([]byte(doc))
		require.NoError(t, err, name)
		// Nothing here is a chain jerboad may write into or a rule it owns.
		assert.Empty(t, foreignForwardChains(objs), name)
		assert.Empty(t, ownedNftRules(objs), name)
	}

	for name, doc := range map[string]string{
		"empty":            ``,
		"not json":         `table inet filter {`,
		"array":            `[]`,
		"nftables object":  `{"nftables": {}}`,
		"nftables string":  `{"nftables": "x"}`,
		"element number":   `{"nftables": [5]}`,
		"chain string":     `{"nftables": [{"chain": "x"}]}`,
		"handle string":    `{"nftables": [{"rule": {"handle": "7"}}]}`,
		"handle too large": `{"nftables": [{"rule": {"handle": 1e400}}]}`,
		"trailing garbage": `{"nftables": []} extra`,
	} {
		_, err := parseNftRuleset([]byte(doc))
		require.Error(t, err, name)
	}
}

func FuzzParseNftRuleset(f *testing.F) {
	f.Add([]byte(sampleNftRuleset))
	f.Add([]byte(sampleNftRuleset[:200]))
	f.Add([]byte(`{"nftables": [{"chain": {"family": "inet", "table": "a b", "name": "c", "hook": "forward"}}]}`))
	f.Add([]byte(`{"nftables": [null, {"rule": {"comment": "jerboa published ports", "handle": -1}}]}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, doc []byte) {
		objs, err := parseNftRuleset(doc)
		if err != nil {
			if objs != nil {
				t.Fatal("objects returned with an error")
			}
			return
		}
		// Chain names go unquoted into nft scripts run as root.
		for _, c := range foreignForwardChains(objs) {
			if (c.Family != "ip" && c.Family != "inet") || !nftIdentifier(c.Table) || !nftIdentifier(c.Name) {
				t.Fatalf("chain %+v would be written to", c)
			}
			if strings.ContainsAny(c.Table+c.Name, " \t\n;{}\"'#\\") {
				t.Fatalf("chain %+v carries script syntax", c)
			}
			if c.Table == natTable {
				t.Fatalf("jerboad's own table was listed as foreign: %+v", c)
			}
		}
		_ = ownedNftRules(objs)
	})
}

func TestNftIdentifier(t *testing.T) {
	for _, ok := range []string{"filter", "FORWARD", "filter_FORWARD", "f1", "a.b-c/d", "x"} {
		assert.True(t, nftIdentifier(ok), ok)
	}
	for _, bad := range []string{
		"", " ", "1filter", "_filter", "-f", ".f", "/f", "odd name", "a;b", "a\nb", "a\tb", "a{b", "a}b", `a"b`, "a'b",
		"a#b", "a\\b", "a$b", "a*b", "fílter", "ñ", "a\x00b", "filter ", " filter",
	} {
		assert.False(t, nftIdentifier(bad), "%q", bad)
	}
}

func TestTapElements_HostileInput(t *testing.T) {
	const mac, ip = "02:00:00:00:00:01", "10.100.0.2"
	for _, tap := range []string{
		"", " ", "tap 0", `tap"0`, "tap0\n", "tap0;", "tap0}", "tap0 }\nflush ruleset\n", "tap.0", "tap/0", "../tap0",
		"jerboa-tap-toolong", "tap\x00", "täp0", "tap0#",
	} {
		_, err := tapElements(tap, mac, ip)
		require.ErrorContains(t, err, "invalid tap name", "%q", tap)
	}
	for _, m := range []string{
		"", "garbage", "02:00:00:00:00", "02:00:00:00:00:01:02", "02:00:00:00:00:00:00:01", "02-00-00-00-00-0g",
		"02:00:00:00:00:01 }", "0200.0000.0001.0002", " 02:00:00:00:00:01",
	} {
		_, err := tapElements("tap0", m, ip)
		require.ErrorContains(t, err, "invalid guest mac", "%q", m)
	}
	for _, addr := range []string{"", "garbage", "fd00::2", "::1", "10.100.0", "10.100.0.2/24", "10.100.0.2 }", "10.100.0.256", " 10.100.0.2"} {
		_, err := tapElements("tap0", mac, addr)
		require.ErrorContains(t, err, "invalid guest ip", "%q", addr)
	}

	// Other spellings of a valid value are written in one canonical form,
	// so the element added and the element deleted later are the same.
	want, err := tapElements("tap0", mac, ip)
	require.NoError(t, err)
	for _, spelling := range [][2]string{
		{"02-00-00-00-00-01", ip}, {"0200.0000.0001", ip}, {"02:00:00:00:00:01", "::ffff:10.100.0.2"}, {"02:00:00:00:00:01", "::ffff:a64:2"},
	} {
		got, err := tapElements("tap0", spelling[0], spelling[1])
		require.NoError(t, err, "%v", spelling)
		assert.Equal(t, want, got, "%v", spelling)
	}
}

// scriptSafe reports whether s holds only what a generated nft script is
// made of: words, numbers, addresses and the punctuation of set elements.
func scriptSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(" .:{}\"_-\n", r):
		default:
			return false
		}
	}
	return true
}

func FuzzTapElements(f *testing.F) {
	f.Add("jerboa-tap-ab12", "02:00:00:00:00:01", "10.100.0.2")
	f.Add("tap0", "02-00-00-00-00-01", "::ffff:10.100.0.2")
	f.Add("tap0 }\nflush ruleset", "02:00:00:00:00:01", "10.100.0.2")
	f.Add("tap0", "02:00:00:00:00:01 }\nflush ruleset", "10.100.0.2 }\nflush ruleset")
	f.Add("", "", "")
	f.Fuzz(func(t *testing.T, tap, mac, ip string) {
		elems, err := tapElements(tap, mac, ip)
		if err != nil {
			if elems != "" {
				t.Fatalf("a refused binding still produced a script: %q", elems)
			}
			return
		}
		if !scriptSafe(elems) {
			t.Fatalf("script carries input text: %q", elems)
		}
		lines := strings.Split(strings.TrimSuffix(elems, "\n"), "\n")
		if len(lines) != 3 || !strings.HasSuffix(elems, "\n") {
			t.Fatalf("expected three elements, got %q", elems)
		}
		for _, l := range lines {
			if !strings.HasPrefix(l, "element bridge "+guardTable+" ") || strings.Count(l, "{") != 1 || strings.Count(l, "}") != 1 {
				t.Fatalf("malformed element %q", l)
			}
		}
		// The tap is listed last when added and first when removed, so it
		// is never guarded without its bindings.
		if !strings.Contains(lines[2], " taps ") {
			t.Fatalf("the taps element is not last: %q", elems)
		}
		reversed := strings.Split(strings.TrimSuffix(reverseLines(elems), "\n"), "\n")
		if len(reversed) != 3 || reversed[0] != lines[2] || reversed[2] != lines[0] {
			t.Fatalf("reverseLines(%q) = %q", elems, reversed)
		}
		if got := prefixLines(elems, "add "); strings.Count(got, "add element") != 3 {
			t.Fatalf("prefixLines lost a line: %q", got)
		}
	})
}

func FuzzNATScript(f *testing.F) {
	f.Add("10.100.0.9", "127.0.0.1", "tcp", uint16(8080), uint16(80))
	f.Add("10.100.0.9", "", "udp", uint16(53), uint16(53))
	f.Add("10.0.0.2 }\nflush ruleset", "127.0.0.1 }\nflush ruleset", "tcp }", uint16(1), uint16(1))
	f.Add("::ffff:10.0.0.2", "::ffff:127.0.0.1", "UDP", uint16(0), uint16(65535))
	f.Fuzz(func(t *testing.T, guest, bind, proto string, hostPort, guestPort uint16) {
		// From what a client may send to what nft is given, the whole way.
		entries, err := natEntries(guest, []PortForward{{HostPort: hostPort, GuestPort: guestPort, Protocol: proto, BindAddr: bind}})
		if err != nil {
			return
		}
		for _, op := range []string{"add", "delete"} {
			script := natScript(op, entries)
			if !scriptSafe(script) {
				t.Fatalf("script carries request text: %q", script)
			}
			if strings.Count(script, "\n") != len(entries) || strings.Count(script, op+" element ip "+natTable+" pub_") != len(entries) {
				t.Fatalf("%d entries became %q", len(entries), script)
			}
		}
	})
}

func TestCheckBridgeCIDR_HostileInput(t *testing.T) {
	require.NoError(t, checkBridgeCIDR("10.100.3.1/24"))
	require.NoError(t, checkBridgeCIDR("172.20.0.1/16"))
	for _, bad := range []string{
		"", "10.100.3.1", "10.100.3.1/", "/24", "10.100.3.1/33", "10.100.3.1/24 ", "10.100.3.1/24; ip link del eth0",
		"10.100.3.0/24",   // the network address is not a gateway
		"10.100.3.2/24",   // a gateway that is not the first address
		"10.100.3.255/24", // broadcast
		"8.8.8.1/24",      // not private
		"10.0.0.1/8",      // wider than a network may be
		"10.100.3.1/30",   // too small
		"10.53.0.1/24",    // holds the reserved DNS address
		"fd00::1/64",
		"::ffff:10.100.3.1/120",
	} {
		require.Error(t, checkBridgeCIDR(bad), "%q", bad)
	}
	// A host route over the range is found here too, not only at creation.
	withHostRoute(t, "10.100.0.0/16")
	require.ErrorContains(t, checkBridgeCIDR("10.100.3.1/24"), "overlaps")
}

func TestManagedNames_HostileInput(t *testing.T) {
	for _, bad := range []string{
		"", "eth0", "docker0", "br0", "jerboa", "xjerboa-br0", " jerboa-br0", "jerboa-br0 ", "jerboa-br0\n", "jerboa-br0\t",
		"jerboa-br/../x", "jerboa-br.100", "jerboa-br-waytoolong",
	} {
		require.Error(t, checkManagedBridgeName(bad), "%q", bad)
	}
	// Every bridge name the store derives is one the bridge code accepts,
	// including for the longest and the shortest network names.
	for _, name := range []string{"a", "app", "abcde", "abcdef", "a-very-long-network-name", strings.Repeat("n", 63)} {
		bridge := deriveBridgeName(name)
		require.NoError(t, checkManagedBridgeName(bridge), "network %q -> bridge %q", name, bridge)
		require.True(t, validIfaceName(bridge), "network %q -> bridge %q", name, bridge)
	}
	for _, bad := range []string{"", "sixteen-chars-ab", "a b", "a/b", "a.b", "a\x00", "tap0\n", "ü"} {
		assert.False(t, validIfaceName(bad), "%q", bad)
	}
}

func TestAddrAlreadyAssigned(t *testing.T) {
	// iproute2 words the harmless failure differently across versions; any
	// other failure must not be taken for success.
	for _, out := range []string{"RTNETLINK answers: File exists", "Error: ipv4: Address already assigned.", "error: ADDRESS ALREADY ASSIGNED"} {
		assert.True(t, addrAlreadyAssigned(out), out)
	}
	for _, out := range []string{"", "RTNETLINK answers: Operation not permitted", "Cannot find device \"jerboa-br0\"", "RTNETLINK answers: No such file or directory"} {
		assert.False(t, addrAlreadyAssigned(out), out)
	}
}
