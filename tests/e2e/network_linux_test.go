//go:build e2e && linux

package e2e

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// requireNetAdmin makes sure the test can create interfaces, send raw
// frames and read the firewall: what the tests of the host side of guest
// networking need beyond what the daemon itself does.
func requireNetAdmin(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"nft", "ip"} {
		if _, err := exec.LookPath(tool); err != nil {
			missing(t, "%s not found in PATH", tool)
		}
	}
	requireGuestNetworking(t)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		missing(t, "raw sockets need CAP_NET_RAW (%v); %s", err, grantNetAdmin)
	}
	_ = unix.Close(fd)
}

func nft(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("nft", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// bridgeOf returns the host bridge of a network.
func (e *env) bridgeOf(network string) string {
	e.t.Helper()
	var nets []struct {
		Name   string `json:"name"`
		Bridge string `json:"bridge"`
	}
	e.jsonOut(&nets, "network", "ls")
	for _, n := range nets {
		if n.Name == network {
			require.NotEmpty(e.t, n.Bridge)
			return n.Bridge
		}
	}
	e.t.Fatalf("network %s is not listed", network)
	return ""
}

func routeLocalnet(t *testing.T, bridge string) string {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + bridge + "/route_localnet")
	require.NoError(t, err)
	return strings.TrimSpace(string(data))
}

// injector puts Ethernet frames on a bridge the way a guest's tap does:
// through a port of the bridge, here one end of a veth pair. The frames
// carry any source and destination the test chooses, which is what a guest
// that controls its own network stack can do.
type injector struct {
	t      *testing.T
	fd     int
	ifidx  int
	srcMAC net.HardwareAddr
	dstMAC net.HardwareAddr // the bridge: frames go to the host's IP stack
}

func newInjector(t *testing.T, bridge string) *injector {
	t.Helper()
	n := suite.seq.Add(1)
	inside, outside := fmt.Sprintf("jbe2e%da", n), fmt.Sprintf("jbe2e%db", n)
	run := func(args ...string) {
		out, err := exec.Command("ip", args...).CombinedOutput()
		require.NoErrorf(t, err, "ip %s: %s", strings.Join(args, " "), out)
	}
	run("link", "add", inside, "type", "veth", "peer", "name", outside)
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", inside).Run() })
	run("link", "set", inside, "master", bridge)
	run("link", "set", inside, "up")
	run("link", "set", outside, "up")

	br, err := net.InterfaceByName(bridge)
	require.NoError(t, err)
	out, err := net.InterfaceByName(outside)
	require.NoError(t, err)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	require.NoError(t, err, "open a raw socket")
	t.Cleanup(func() { _ = unix.Close(fd) })
	return &injector{t: t, fd: fd, ifidx: out.Index, srcMAC: net.HardwareAddr{0x02, 0xe2, 0xe0, 0, 0, byte(n)}, dstMAC: br.HardwareAddr}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// ipChecksum is the Internet checksum of an IPv4 header.
func ipChecksum(hdr []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(hdr); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(hdr[i:]))
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// sendUDP injects one UDP datagram from src to dst:port.
func (in *injector) sendUDP(src, dst net.IP, port int, payload string) {
	in.t.Helper()
	udp := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(udp[0:], 40000)
	binary.BigEndian.PutUint16(udp[2:], uint16(port))
	binary.BigEndian.PutUint16(udp[4:], uint16(len(udp)))
	// Checksum 0: none, which IPv4 allows.
	copy(udp[8:], payload)

	ip := make([]byte, 20)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(20+len(udp)))
	ip[8] = 64
	ip[9] = unix.IPPROTO_UDP
	copy(ip[12:16], src.To4())
	copy(ip[16:20], dst.To4())
	binary.BigEndian.PutUint16(ip[10:], ipChecksum(ip))

	frame := make([]byte, 0, 14+len(ip)+len(udp))
	frame = append(frame, in.dstMAC...)
	frame = append(frame, in.srcMAC...)
	frame = append(frame, 0x08, 0x00)
	frame = append(frame, ip...)
	frame = append(frame, udp...)

	addr := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: in.ifidx, Halen: 6}
	copy(addr.Addr[:], in.dstMAC)
	require.NoError(in.t, unix.Sendto(in.fd, frame, 0, addr), "inject a frame")
}

// udpSink is a UDP socket on the host that records what reaches it.
type udpSink struct {
	conn *net.UDPConn
	got  chan string
}

func newUDPSink(t *testing.T, ip net.IP) *udpSink {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	require.NoErrorf(t, err, "listen on %s", ip)
	t.Cleanup(func() { _ = conn.Close() })
	s := &udpSink{conn: conn, got: make(chan string, 64)}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			s.got <- string(buf[:n])
		}
	}()
	return s
}

func (s *udpSink) port() int { return s.conn.LocalAddr().(*net.UDPAddr).Port }

// receives reports whether want arrives within d.
func (s *udpSink) receives(want string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case got := <-s.got:
			if got == want {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// guardedNetwork is a daemon publishing ports through kernel NAT, with one
// guest on a network, and an injector on that network's bridge.
type guardedNetwork struct {
	e       *env
	web     *guest
	bridge  string
	gateway net.IP
	spoof   net.IP // an address of the subnet no guest holds
	inj     *injector
	onGW    *udpSink // host socket on the bridge's own address
	onLo    *udpSink // host socket on 127.0.0.1
}

func newGuardedNetwork(t *testing.T) *guardedNetwork {
	t.Helper()
	requireNetAdmin(t)
	// nat, not auto: without kernel NAT the daemon must refuse to start
	// rather than fall back to the proxy, where there is no route_localnet
	// and nothing here would be tested.
	e := newEnv(t, withDaemonArgs("--port-publish", "nat"))
	e.buildProbe()
	netName, subnet := e.network()
	n := &guardedNetwork{e: e}
	n.web = e.startGuest(guestOpts{name: "web", network: netName, env: []string{"PROBE_MARKER=web"}})
	n.bridge = e.bridgeOf(netName)
	n.gateway = net.ParseIP(e.inspect(n.web.id).GatewayIP).To4()
	require.NotNil(t, n.gateway, "the guest has no gateway")
	_, ipnet, err := net.ParseCIDR(subnet)
	require.NoError(t, err)
	n.spoof = net.IPv4(ipnet.IP[0], ipnet.IP[1], ipnet.IP[2], 200).To4()
	n.inj = newInjector(t, n.bridge)
	n.onGW = newUDPSink(t, n.gateway)
	n.onLo = newUDPSink(t, net.IPv4(127, 0, 0, 1))
	return n
}

// requireLoopbackClosed injects datagrams for a socket on the bridge's own
// address (the control: they must arrive, or the injection does not work
// and the rest proves nothing) and for a socket on 127.0.0.1, which must
// receive none.
func (n *guardedNetwork) requireLoopbackClosed(when string) {
	t := n.e.t
	t.Helper()
	arrived := false
	for i := 0; i < 10 && !arrived; i++ {
		n.inj.sendUDP(n.spoof, n.gateway, n.onGW.port(), "control")
		arrived = n.onGW.receives("control", 500*time.Millisecond)
	}
	require.Truef(t, arrived, "%s: a datagram injected on %s for the bridge's own address %s never arrived; "+
		"the injection does not work on this host (a firewall that drops input from jerboa bridges?), so the loopback check would prove nothing",
		when, n.bridge, n.gateway)

	for i := 0; i < 10; i++ {
		n.inj.sendUDP(n.spoof, net.IPv4(127, 0, 0, 1), n.onLo.port(), "attack")
		n.inj.sendUDP(n.spoof, net.IPv4(127, 0, 0, 53), n.onLo.port(), "attack")
	}
	require.Falsef(t, n.onLo.receives("attack", 2*time.Second),
		"%s: a datagram sent from the guest network to 127.0.0.1 reached a socket on the host's loopback", when)
}

// TestKernelNATKeepsGuestsOffHostLoopback checks the two sides of published
// ports under kernel NAT with a guest running. A port published on
// 127.0.0.1 answers there, which needs route_localnet on the guest's
// bridge; and route_localnet must not let anything on the guest network
// reach the host's loopback, which only the guard's nftables rule
// prevents. The test sends, from a port of the bridge, datagrams addressed
// to 127.0.0.1, as a hostile guest can.
func TestKernelNATKeepsGuestsOffHostLoopback(t *testing.T) {
	n := newGuardedNetwork(t)
	e := n.e

	// The published port works from the host's loopback, and only there.
	require.Equal(t, "web", n.web.status().Marker)
	require.False(t, tcpOpen(fmt.Sprintf("%s:%d", hostAddress(t), n.web.port)), "a port published on 127.0.0.1 answers on another address")
	require.Equal(t, "1", routeLocalnet(t, n.bridge), "kernel NAT publishes on 127.0.0.1 through route_localnet")

	// The guard is loaded, and binds the guest's tap to the guest's address.
	out, err := nft(t, "list", "table", "inet", "jerboa_guard")
	require.NoError(t, err, "the guard table must be loaded while guests run")
	require.Contains(t, out, "ip daddr 127.0.0.0/8 drop")
	out, err = nft(t, "list", "set", "bridge", "jerboa_guard", "tap_ip")
	require.NoError(t, err)
	require.Contains(t, out, e.inspect(n.web.id).IPAddress, "the guest's tap must be bound to its address")

	n.requireLoopbackClosed("with a guest running")

	// With the VM gone its translation is gone too.
	e.stopAndRemove(n.web.id)
	require.Eventually(t, func() bool { return !tcpOpen(fmt.Sprintf("127.0.0.1:%d", n.web.port)) }, 15*time.Second, 200*time.Millisecond,
		"the published port of a removed VM still accepts connections")
	out, err = nft(t, "list", "table", "ip", "jerboa")
	require.NoError(t, err)
	require.NotContains(t, out, fmt.Sprintf(". %d :", n.web.port), "the NAT table still translates the removed VM's port")
}

// TestGuardComesBackAfterItIsRemoved removes the guard's table behind the
// daemon, as a reload of the host's firewall does ("flush ruleset"; here
// only Jerboa's own table is deleted). While the guard is gone the daemon
// must take route_localnet away from the bridge, so the host's loopback
// stays closed; then it must reinstall the guard and published ports must
// answer on 127.0.0.1 again.
func TestGuardComesBackAfterItIsRemoved(t *testing.T) {
	slow(t)
	n := newGuardedNetwork(t)
	require.Equal(t, "1", routeLocalnet(t, n.bridge))
	n.requireLoopbackClosed("before the guard is removed")

	_, err := nft(t, "delete", "table", "inet", "jerboa_guard")
	require.NoError(t, err)

	// From here on the bridge may keep route_localnet without the guard only
	// for as long as the daemon takes to notice (it looks every 5 seconds).
	// Watched until the guard has been back, with route_localnet, for a while.
	guardLoaded := func() bool {
		out, err := nft(t, "list", "chain", "inet", "jerboa_guard", "prerouting")
		return err == nil && strings.Contains(out, "ip daddr 127.0.0.0/8 drop")
	}
	deadline := time.Now().Add(40 * time.Second)
	exposed := time.Time{}
	var backSince time.Time
	for time.Now().Before(deadline) {
		loaded := guardLoaded()
		open := routeLocalnet(t, n.bridge) == "1"
		switch {
		case !loaded && open:
			// The daemon checks every 5 seconds: tolerate that window, not more.
			if exposed.IsZero() {
				exposed = time.Now()
			}
			require.Lessf(t, time.Since(exposed), 12*time.Second, "the guard is gone and %s still has route_localnet", n.bridge)
		case loaded && open:
			if backSince.IsZero() {
				backSince = time.Now()
			}
		}
		if !backSince.IsZero() && time.Since(backSince) > 3*time.Second {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	require.Falsef(t, backSince.IsZero(), "the daemon did not reinstall the guard and restore route_localnet within 40s; log:\n%s", n.e.daemonLog())

	n.requireLoopbackClosed("after the guard came back")
	require.Equal(t, "web", n.web.status().Marker, "the published port must answer on 127.0.0.1 again")
}
