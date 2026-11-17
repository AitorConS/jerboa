//go:build e2e

package e2e

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// hostAddress returns an IPv4 address of this host that is not loopback.
func hostAddress(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		// Not an address the daemons under test gave a bridge or the guest
		// DNS: the host's own, as another machine would dial it.
		if ip := ipn.IP.To4(); ip[0] == 10 && (ip[1] >= 209 || ip[1] == 53) {
			continue
		}
		return ipn.IP.String()
	}
	missing(t, "this host has no IPv4 address besides loopback")
	return ""
}

// tcpOpen reports whether something accepts TCP connections at addr.
func tcpOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// TestPublishedPorts checks `run -p` from the host's side: the port serves
// the guest, binds only the address it was given, cannot be taken twice,
// and is released when the VM stops.
func TestPublishedPorts(t *testing.T) {
	e := newEnv(t)
	e.buildProbe()
	netName, _ := e.network()
	hostIP := hostAddress(t)

	// Published on loopback: served there and nowhere else.
	lo := e.startGuest(guestOpts{name: "lo", network: netName, env: []string{"PROBE_MARKER=lo"}})
	require.Equal(t, "lo", lo.status().Marker)
	require.Falsef(t, tcpOpen(fmt.Sprintf("%s:%d", hostIP, lo.port)),
		"a port published on 127.0.0.1 answers on %s", hostIP)

	// The same host port for a second VM is refused, and the refusal leaves
	// neither a running VM nor the first one's port broken.
	res := e.run("run", "probe", "--name", "thief", "--network", netName, "-p", fmt.Sprintf("127.0.0.1:%d:8080", lo.port))
	if res.code == 0 {
		e.vmIDs[strings.TrimSpace(res.stdout)] = true
	}
	require.Equalf(t, 1, res.code, "publishing a host port that is taken must fail:\n%s", res.all())
	// The daemon ends the VM it had started once the port turned out to be
	// taken; `run` returns as that begins, so the listing may still show the
	// VM for a moment.
	require.Eventuallyf(t, func() bool { return e.vmState("thief") != "running" }, 15*time.Second, 100*time.Millisecond,
		"the VM whose port could not be published keeps running:\n%s", res.all())
	for _, v := range e.ps() {
		if v.Name == "thief" {
			e.vmIDs[v.ID] = true
			e.waitNoVMM(v.ID, 15*time.Second)
		}
	}
	require.Equal(t, "lo", lo.status().Marker, "the first VM's port must keep serving")

	// Published on every address: served on loopback and on the host's own.
	anyPort := freePort(t)
	id := strings.TrimSpace(e.ok("run", "probe", "--name", "any", "--network", netName, "-p", fmt.Sprintf("%d:8080", anyPort), "-e", "PROBE_MARKER=any"))
	e.vmIDs[id] = true
	anyGuest := &guest{e: e, id: id, name: "any", port: anyPort}
	anyGuest.waitReady()
	code, body, err := httpGet(fmt.Sprintf("http://%s:%d/", hostIP, anyPort))
	require.NoErrorf(t, err, "a port published on every address must answer on %s", hostIP)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, `"marker":"any"`)

	// Stopping the VM releases its port, and another VM can publish it.
	e.stopAndRemove(lo.id)
	require.Eventually(t, func() bool { return !tcpOpen(fmt.Sprintf("127.0.0.1:%d", lo.port)) }, 15*time.Second, 200*time.Millisecond,
		"the published port of a stopped VM still accepts connections")
	id = strings.TrimSpace(e.ok("run", "probe", "--name", "heir", "--network", netName, "-p", fmt.Sprintf("127.0.0.1:%d:8080", lo.port), "-e", "PROBE_MARKER=heir"))
	e.vmIDs[id] = true
	heir := &guest{e: e, id: id, name: "heir", port: lo.port}
	heir.waitReady()
	require.Equal(t, "heir", heir.status().Marker)
}

// TestNetworksIsolateGuests runs guests on two networks. Guests of one
// network reach each other by address and by name; a guest of the other
// network is neither reachable nor resolvable.
func TestNetworksIsolateGuests(t *testing.T) {
	e := newEnv(t)
	e.buildProbe()
	netA, _ := e.network()
	netB, _ := e.network()
	a1 := e.startGuest(guestOpts{name: "a1", network: netA})
	a2 := e.startGuest(guestOpts{name: "a2", network: netA, env: []string{"PROBE_MARKER=a2"}})
	b1 := e.startGuest(guestOpts{name: "b1", network: netB, env: []string{"PROBE_MARKER=b1"}})
	a2IP, b1IP := e.inspect(a2.id).IPAddress, e.inspect(b1.id).IPAddress
	require.NotEmpty(t, a2IP)
	require.NotEmpty(t, b1IP)

	// The control: the same request, to a guest of the same network, works.
	require.Contains(t, a1.get("/fetch?url="+url.QueryEscape("http://"+a2IP+":8080/")), `"marker":"a2"`)
	require.Equal(t, []string{a2IP}, a1.resolve("a2").Addrs)

	res := a1.dial(b1IP + ":8080")
	require.Falsef(t, res.OK, "a guest on %s connected to a guest on %s", netA, netB)
	lookup := a1.resolve("b1")
	require.Emptyf(t, lookup.Addrs, "a guest on %s resolved the name of a guest on %s", netA, netB)
	// And the other way round.
	require.False(t, b1.dial(a2IP+":8080").OK)
	require.Empty(t, b1.resolve("a2").Addrs)

	// The names the CLI resolves are the ones the guests get.
	var records []struct {
		Name    string `json:"name"`
		Network string `json:"network"`
		IP      string `json:"ip"`
	}
	e.jsonOut(&records, "dns", "list")
	got := map[string]string{}
	for _, r := range records {
		got[r.Network+"/"+r.Name] = r.IP
	}
	require.Equal(t, a2IP, got[netA+"/a2"])
	require.Equal(t, b1IP, got[netB+"/b1"])

	// A stopped guest's name goes away with it.
	e.stopAndRemove(a2.id)
	require.Eventually(t, func() bool { return len(a1.resolve("a2").Addrs) == 0 }, 15*time.Second, 500*time.Millisecond,
		"the name of a removed VM still resolves")
}

// fakeUpstream is the resolver the daemon forwards to: it answers names
// under fast.test and never answers names under slow.test.
type fakeUpstream struct {
	conn *net.UDPConn
	mu   sync.Mutex
	slow int // questions under slow.test received
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	u := &fakeUpstream{conn: conn}
	go u.serve()
	return u
}

func (u *fakeUpstream) addr() string { return u.conn.LocalAddr().String() }

func (u *fakeUpstream) slowSeen() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.slow
}

func (u *fakeUpstream) serve() {
	buf := make([]byte, 2048)
	for {
		n, src, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var p dnsmessage.Parser
		hdr, err := p.Start(buf[:n])
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil {
			continue
		}
		name := strings.ToLower(q.Name.String())
		if strings.HasSuffix(name, "slow.test.") {
			u.mu.Lock()
			u.slow++
			u.mu.Unlock()
			continue
		}
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: hdr.ID, Response: true, RecursionAvailable: true})
		b.EnableCompression()
		if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil {
			continue
		}
		if q.Type == dnsmessage.TypeA && strings.HasSuffix(name, "fast.test.") {
			_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 1}, dnsmessage.AResource{A: [4]byte{203, 0, 113, 7}})
		}
		if resp, err := b.Finish(); err == nil {
			_, _ = u.conn.WriteToUDP(resp, src)
		}
	}
}

// TestGuestDNSForwardingLimits checks the limit on what one guest can make
// the daemon forward. A guest floods the resolver with names whose upstream
// never answers; meanwhile another guest must still get its own names
// resolved, and the upstream must not receive more questions from the
// flooder than the per-guest limit allows.
func TestGuestDNSForwardingLimits(t *testing.T) {
	upstream := newFakeUpstream(t)
	e := newEnv(t, withDaemonEnv("JERBOA_DNS_UPSTREAM="+upstream.addr()))
	e.buildProbe()
	netName, _ := e.network()
	flooder := e.startGuest(guestOpts{name: "flooder", network: netName})
	victim := e.startGuest(guestOpts{name: "victim", network: netName})

	// The control: forwarding works at all.
	require.Equal(t, []string{"203.0.113.7"}, victim.resolve("before.fast.test").Addrs)

	type floodResult struct {
		Answered int `json:"answered"`
		Failed   int `json:"failed"`
	}
	const names = 400
	floodDone := make(chan floodResult, 1)
	start := time.Now()
	go func() {
		var fr floodResult
		code, body, err := httpGet(flooder.url(fmt.Sprintf("/dnsflood?count=%d&suffix=slow.test", names)))
		if err == nil && code == http.StatusOK {
			_ = jsonUnmarshal(body, &fr)
		}
		floodDone <- fr
	}()

	// In the middle of the flood the other guest resolves as if alone.
	require.Eventually(t, func() bool { return upstream.slowSeen() > 0 }, 10*time.Second, 50*time.Millisecond, "the flood never reached the upstream")
	for i := 0; i < 5; i++ {
		res := victim.resolve(fmt.Sprintf("during-%d.fast.test", i))
		require.Equalf(t, []string{"203.0.113.7"}, res.Addrs, "lookup %d of another guest during the flood failed: %s", i, res.Error)
		require.Lessf(t, res.MS, int64(2500), "lookup %d of another guest during the flood took %dms", i, res.MS)
		// Names of the network do not need the upstream at all.
		require.NotEmpty(t, victim.resolve("flooder").Addrs)
	}

	var fr floodResult
	select {
	case fr = <-floodDone:
	case <-time.After(60 * time.Second):
		t.Fatal("the flood did not end")
	}
	elapsed := time.Since(start)
	require.Equal(t, names, fr.Answered+fr.Failed, "the flooder did not report on every lookup")

	// One guest holds at most 8 forwarded questions at a time, each for at
	// most the 3 seconds the daemon waits for the upstream.
	const perGuest, upstreamTimeout = 8, 3 * time.Second
	allowed := perGuest * (int(elapsed/upstreamTimeout) + 2)
	require.LessOrEqualf(t, upstream.slowSeen(), allowed,
		"the upstream received %d questions from one guest in %s; the limit allows %d", upstream.slowSeen(), elapsed.Round(time.Millisecond), allowed)

	// Once the flood is over the flooder itself resolves again.
	require.Eventually(t, func() bool {
		return len(flooder.resolve("after.fast.test").Addrs) == 1
	}, 20*time.Second, time.Second, "the flooder cannot resolve after its flood ended")
}
