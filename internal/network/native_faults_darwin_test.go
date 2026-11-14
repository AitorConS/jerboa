package network

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The control socket of a direct VM-to-VM link is spoken by VMM processes
// that can crash, hang up mid-message or send nonsense, and the native
// network is set up from paths and addresses that may be unusable. A bad
// peer costs only its own links; a failed setup leaves nothing half open.

func (f *peerFixture) session(m *peerMember) *peerSession {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	return m.session
}

func (f *peerFixture) memberPairs(m *peerMember) int {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	return m.pairs
}

// hungUp reports whether the switch closed the connection (rather than the
// read merely timing out with the connection still open).
func hungUp(t *testing.T, c *net.UnixConn) bool {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	var b [peerMessageSize]byte
	for {
		_, err := c.Read(b[:])
		if err == nil {
			continue // messages queued before the close
		}
		var ne net.Error
		return !errors.As(err, &ne) || !ne.Timeout()
	}
}

// paired links a and b and returns their VMMs with the pair acknowledged.
func (f *peerFixture) paired(t *testing.T, a, b *peerMember, pa, pb string) (va, vb *fakeVMM) {
	t.Helper()
	va, ea := connect(t, a, pa)
	vb, eb := connect(t, b, pb)
	f.p.observe(a, b.mac)
	ma, fa := va.expect(peerAdd)
	mb, fb := vb.expect(peerAdd)
	t.Cleanup(func() { _ = unix.Close(fa); _ = unix.Close(fb) })
	linked(t, fa, fb)
	va.send(peerMessage{Type: peerAck, Flags: 1, Epoch: ea, Value: ma.Value})
	vb.send(peerMessage{Type: peerAck, Flags: 1, Epoch: eb, Value: mb.Value})
	waitFor(t, func() bool {
		f.p.mu.Lock()
		defer f.p.mu.Unlock()
		pr := f.p.byID[ma.Value]
		return pr != nil && pr.acked[0] && pr.acked[1]
	})
	return va, vb
}

func TestPeeringFaults_BadHelloNeverBecomesASession(t *testing.T) {
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	hello := peerMessage{Type: peerHello, Value: peerFeatures, MAC: a.mac}.encode()

	for name, first := range map[string][]byte{
		"garbage":          bytes.Repeat([]byte{0xa5}, peerMessageSize),
		"zeroes":           make([]byte, peerMessageSize),
		"not a hello":      func() []byte { b := peerMessage{Type: peerAck, Flags: 1, MAC: a.mac}.encode(); return b[:] }(),
		"welcome from VMM": func() []byte { b := peerMessage{Type: peerWelcome, MAC: a.mac}.encode(); return b[:] }(),
		"another VM's MAC": func() []byte {
			b := peerMessage{Type: peerHello, Value: peerFeatures, MAC: [6]byte{2, 0, 0, 0, 0, 9}}.encode()
			return b[:]
		}(),
		"reserved byte set": func() []byte { b := hello; b[6] = 1; return b[:] }(),
		"wrong magic":       func() []byte { b := hello; b[3] = '2'; return b[:] }(),
		"two hellos glued":  append(hello[:16:16], hello[:16]...),
	} {
		t.Run(name, func(t *testing.T) {
			c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: pa, Net: "unix"})
			require.NoError(t, err)
			defer c.Close()
			_, err = c.Write(first)
			require.NoError(t, err)
			// The switch hangs up without a word: no welcome, no session,
			// nothing a later pairing could be sent to.
			require.True(t, hungUp(t, c), "the connection was kept open")
			require.Nil(t, f.session(a))
		})
	}

	t.Run("hello cut short", func(t *testing.T) {
		// The VMM dies part-way through its first message, at every length.
		for n := range peerMessageSize {
			c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: pa, Net: "unix"})
			require.NoError(t, err)
			_, err = c.Write(hello[:n])
			require.NoError(t, err)
			require.NoError(t, c.Close())
		}
		require.Nil(t, f.session(a))
	})

	// After all of that the genuine VMM still gets in.
	_, epoch := connect(t, a, pa)
	require.NotZero(t, epoch)
	require.NotNil(t, f.session(a))
}

func TestPeeringFaults_VMMMisbehavesWithALinkUp(t *testing.T) {
	for name, misbehave := range map[string]func(t *testing.T, va *fakeVMM){
		"hangs up": func(_ *testing.T, va *fakeVMM) { _ = va.conn.Close() },
		"sends garbage": func(t *testing.T, va *fakeVMM) {
			_, err := va.conn.Write(bytes.Repeat([]byte{0xff}, peerMessageSize))
			require.NoError(t, err)
		},
		"sends an unknown type": func(t *testing.T, va *fakeVMM) {
			b := peerMessage{Type: peerAck}.encode()
			b[4] = 200
			_, err := va.conn.Write(b[:])
			require.NoError(t, err)
		},
		"dies mid-message": func(t *testing.T, va *fakeVMM) {
			b := peerMessage{Type: peerStats, Epoch: 5, Value: 6}.encode()
			_, err := va.conn.Write(b[:20])
			require.NoError(t, err)
			require.NoError(t, va.conn.Close())
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPeerFixture(t)
			a, pa := f.member(t, 1)
			b, pb := f.member(t, 2)
			va, vb := f.paired(t, a, b, pa, pb)
			require.Equal(t, 1, f.pairs())

			misbehave(t, va)

			// The partner is told to drop the link at once: it must not go
			// on sending frames into a socket nobody reads.
			del, fd := vb.expect(peerDel)
			require.Equal(t, -1, fd)
			require.NotZero(t, del.Value)
			waitFor(t, func() bool { return f.session(a) == nil })
			require.Zero(t, f.pairs())
			require.Zero(t, f.memberPairs(a))
			require.Zero(t, f.memberPairs(b))
			// The well-behaved VMM keeps its session and can pair again
			// once the other comes back.
			require.NotNil(t, f.session(b))
			va2, _ := connect(t, a, pa)
			f.p.observe(b, a.mac)
			_, fa := va2.expect(peerAdd)
			_, fb := vb.expect(peerAdd)
			linked(t, fb, fa)
			_ = unix.Close(fa)
			_ = unix.Close(fb)
		})
	}
}

func TestPeeringFaults_HarmlessMessagesKeepTheSession(t *testing.T) {
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	b, pb := f.member(t, 2)
	va, vb := f.paired(t, a, b, pa, pb)
	ea := f.session(a).epoch

	// Messages only the switch sends, acknowledgements for links that do not
	// exist, statistics: none is a reason to drop a working link.
	va.send(peerMessage{Type: peerWelcome, Epoch: ea})
	va.send(peerMessage{Type: peerAdd, Epoch: ea, Value: 999})
	va.send(peerMessage{Type: peerDel, Epoch: ea, Value: 999})
	va.send(peerMessage{Type: peerReset})
	va.send(peerMessage{Type: peerHello, Value: peerFeatures, MAC: a.mac})
	va.send(peerMessage{Type: peerAck, Flags: 1, Epoch: ea, Value: 999})
	va.send(peerMessage{Type: peerDead, Epoch: ea, Value: 999})
	va.send(peerMessage{Type: peerIdle, Epoch: ea + 1, Value: 1})
	va.send(peerMessage{Type: peerStats, Epoch: 1234, Value: 5678})
	waitFor(t, func() bool { return a.tx.Load() == 1234 && a.rx.Load() == 5678 })

	vb.silent()
	require.Equal(t, 1, f.pairs())
	require.NotNil(t, f.session(a))
	require.Equal(t, 1, f.memberPairs(a))
}

func TestPeeringFaults_VMRemovedWithALinkUp(t *testing.T) {
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	b, pb := f.member(t, 2)
	va, vb := f.paired(t, a, b, pa, pb)

	f.p.unregister(a)
	vb.expect(peerDel)
	require.True(t, hungUp(t, va.conn), "the removed VM's control connection stayed open")
	require.Zero(t, f.pairs())
	require.Zero(t, f.memberPairs(b))

	// Frames still addressed to the removed VM do not pair with a ghost,
	// and removing it twice is harmless.
	f.p.observe(b, a.mac)
	f.p.observe(a, b.mac)
	f.p.unregister(a)
	vb.silent()
	require.Zero(t, f.pairs())
	f.p.mu.Lock()
	require.Empty(t, f.p.byID)
	require.NotContains(t, f.p.members, a.mac)
	f.p.mu.Unlock()
}

func TestPeeringFaults_ObserveWithoutAUsablePeer(t *testing.T) {
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	b, _ := f.member(t, 2)
	va, _ := connect(t, a, pa)

	f.p.observe(a, a.mac)                                       // itself
	f.p.observe(a, [6]byte{2, 0, 0, 0, 0, 77})                  // not on this network
	f.p.observe(a, [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) // broadcast
	f.p.observe(a, b.mac)                                       // b's VMM never connected
	f.p.observe(b, a.mac)
	va.silent()
	require.Zero(t, f.pairs())
	require.Zero(t, f.memberPairs(a))
}

func TestPeeringFaults_ReRegisteredVMReplacesItsOldSession(t *testing.T) {
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	b, pb := f.member(t, 2)
	va, vb := f.paired(t, a, b, pa, pb)

	// The VM restarts with the same MAC while its old VMM is still
	// connected: the old session and its links go, and the old VMM cannot
	// act for the new VM.
	again := f.p.register(net.HardwareAddr(a.mac[:]), net.IPv4(10, 0, 0, 1))
	vb.expect(peerDel)
	require.True(t, hungUp(t, va.conn))
	require.Zero(t, f.pairs())
	require.Nil(t, f.session(again))
	f.p.mu.Lock()
	require.Same(t, again, f.p.members[a.mac])
	f.p.mu.Unlock()
}

func TestNativeFaults_InvalidSubnetOrGateway(t *testing.T) {
	for _, tc := range [][2]string{
		{"", ""}, {"garbage", "10.0.0.1"}, {"10.0.0.0", "10.0.0.1"}, {"10.0.0.0/33", "10.0.0.1"},
		{"10.0.0.0/24", ""}, {"10.0.0.0/24", "garbage"}, {"10.0.0.0/24", "10.0.1.1"}, {"10.0.0.0/24", "10.0.0.1/24"},
		{"fd00::/64", "fd00::1"}, {"fd00::/64", "10.0.0.1"}, {"10.0.0.0/24", "fd00::1"},
	} {
		n, err := NewNativeNetwork(tc[0], tc[1], nil)
		require.Error(t, err, "%q / %q", tc[0], tc[1])
		require.Nil(t, n)
		n, err = NewNativeNetworkWithPolicy(tc[0], tc[1], nil, func(string, string, uint16, bool) bool { return true })
		require.Error(t, err, "%q / %q", tc[0], tc[1])
		require.Nil(t, n)
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	// Unix socket paths are limited to 104 bytes on macOS, less than a
	// test's own temporary directory often is.
	dir, err := os.MkdirTemp("/tmp", "jnat")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestNativeFaults_ListenVMFailuresLeaveNothingBehind(t *testing.T) {
	n, err := NewNativeNetworkWithPolicy("172.29.0.0/24", "172.29.0.1", nil, func(string, string, uint16, bool) bool { return true })
	require.NoError(t, err)
	defer n.Close()
	dir := shortDir(t)
	const ip, mac = "172.29.0.2", "02:00:00:00:00:02"

	t.Run("directory missing", func(t *testing.T) {
		l, err := n.ListenVM(filepath.Join(dir, "gone", "vm.sock"), ip, mac)
		require.Error(t, err)
		require.Nil(t, l)
		require.Nil(t, n.egress.reserve(ip), "a VM that never got its link may not open host connections")
	})
	t.Run("path held by a live socket", func(t *testing.T) {
		path := filepath.Join(dir, "busy.sock")
		other, err := net.Listen("unix", path)
		require.NoError(t, err)
		defer other.Close()
		l, err := n.ListenVM(path, ip, mac)
		require.Error(t, err)
		require.Nil(t, l)
		// The socket that was there still answers.
		c, err := net.Dial("unix", path)
		require.NoError(t, err)
		require.NoError(t, c.Close())
		require.Nil(t, n.egress.reserve(ip))
	})
	t.Run("control socket cannot be created", func(t *testing.T) {
		if !NativePeeringEnabled() {
			t.Skip("direct links are disabled")
		}
		path := filepath.Join(dir, "vm.sock")
		// Something that cannot be removed sits where the control socket goes.
		control := PeerControlPath(path)
		require.NoError(t, os.Mkdir(control, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(control, "keep"), nil, 0o600))

		l, err := n.ListenVM(path, ip, mac)
		require.Error(t, err)
		require.Nil(t, l)
		// The Ethernet socket opened first was closed again, and the VM's
		// egress allowance withdrawn: nothing serves a VM that has no link.
		_, err = net.DialTimeout("unix", path, time.Second)
		require.Error(t, err, "the stream socket was left listening")
		require.Nil(t, n.egress.reserve(ip))
		n.peers.mu.Lock()
		require.Empty(t, n.peers.members)
		n.peers.mu.Unlock()
	})
	t.Run("then a usable path works", func(t *testing.T) {
		path := filepath.Join(dir, "ok.sock")
		l, err := n.ListenVM(path, ip, mac)
		require.NoError(t, err)
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the VM's link must not be open to other users")
		f := n.egress.reserve(ip)
		require.NotNil(t, f)
		f.close()

		// A VMM is connected when the link is closed: it is hung up on,
		// the VM's allowance ends, and closing twice is harmless.
		vmm, err := net.Dial("unix", path)
		require.NoError(t, err)
		defer vmm.Close()
		require.Eventually(t, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.conn != nil }, 10*time.Second, time.Millisecond)
		l.Close()
		l.Close()
		require.NoError(t, vmm.SetReadDeadline(time.Now().Add(10*time.Second)))
		_, err = readToEnd(vmm)
		var ne net.Error
		require.False(t, errors.As(err, &ne) && ne.Timeout(), "the VMM connection outlived its link")
		require.Nil(t, n.egress.reserve(ip))
		_, err = net.DialTimeout("unix", path, time.Second)
		require.Error(t, err)
	})
}

// readToEnd reads c until it fails and returns the error that ended it.
func readToEnd(c net.Conn) (int64, error) {
	buf := make([]byte, 4096)
	var total int64
	for {
		n, err := c.Read(buf)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
}

func TestNativeFaults_DialContextBadAddresses(t *testing.T) {
	n, err := NewNativeNetwork("172.29.0.0/24", "172.29.0.1", nil)
	require.NoError(t, err)
	defer n.Close()
	for _, addr := range []string{
		"", "172.29.0.2", "172.29.0.2:", ":80", "172.29.0.2:notaport", "172.29.0.2:99999", "172.29.0.2:-1",
		"guest.internal:80", "[fd00::2]:80", "172.29.0.2:80:80", "172.29.0.256:80",
	} {
		c, err := n.DialContext(context.Background(), "tcp", addr)
		require.Error(t, err, "%q", addr)
		require.Nil(t, c, "%q", addr)
	}

	// A guest that is not there (not booted yet, or gone): the dial ends
	// with the caller's deadline instead of waiting on its own.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	c, err := n.DialContext(ctx, "tcp", "172.29.0.9:8080")
	require.Error(t, err)
	require.Nil(t, c)
	require.Less(t, time.Since(began), 10*time.Second)
}

func TestNativeFaults_ExposeConflictsAndBadInput(t *testing.T) {
	n, err := NewNativeNetworkWithPolicy("172.29.0.0/24", "172.29.0.1", nil, func(string, string, uint16, bool) bool { return true })
	require.NoError(t, err)
	defer n.Close()
	n.egress.activateSource("172.29.0.2")

	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	busyAddr := busy.Addr().String()
	busyUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer busyUDP.Close()

	// A port another process holds: the publication fails and is not
	// remembered, so it can be made once the port is free.
	require.Error(t, n.Expose("tcp", busyAddr, "172.29.0.2:80"))
	require.Error(t, n.Expose("udp", busyUDP.LocalAddr().String(), "172.29.0.2:53"))
	n.publishMu.Lock()
	require.Empty(t, n.published)
	n.publishMu.Unlock()
	require.NoError(t, busy.Close())
	require.NoError(t, n.Expose("tcp", busyAddr, "172.29.0.2:80"))
	require.ErrorContains(t, n.Expose("tcp", busyAddr, "172.29.0.3:81"), "already exposed")

	for name, tc := range map[string][3]string{
		"unknown protocol":      {"sctp", "127.0.0.1:0", "172.29.0.2:80"},
		"empty protocol":        {"", "127.0.0.1:0", "172.29.0.2:80"},
		"remote without a port": {"tcp", "127.0.0.1:0", "172.29.0.2"},
		"remote garbage":        {"udp", "127.0.0.1:0", "garbage"},
		"local garbage":         {"tcp", "not an address", "172.29.0.2:80"},
		"local port too large":  {"udp", "127.0.0.1:70000", "172.29.0.2:53"},
		"udp guest port named":  {"udp", "127.0.0.1:0", "172.29.0.2:dns"},
		"udp guest port large":  {"udp", "127.0.0.1:0", "172.29.0.2:70000"},
	} {
		require.Error(t, n.Expose(tc[0], tc[1], tc[2]), name)
	}
	n.publishMu.Lock()
	require.Len(t, n.published, 1, "a refused publication was remembered")
	n.publishMu.Unlock()

	// Unpublishing frees the port; unpublishing what was never published,
	// or twice, is not an error.
	require.NoError(t, n.Unexpose("tcp", busyAddr))
	require.NoError(t, n.Unexpose("tcp", busyAddr))
	require.NoError(t, n.Unexpose("udp", "127.0.0.1:1"))
	ln, err := net.Listen("tcp4", busyAddr)
	require.NoError(t, err, "the published port stayed bound")
	require.NoError(t, ln.Close())
}

func TestNativeFaults_CloseEndsPublishedPortsAndIsIdempotent(t *testing.T) {
	n, err := NewNativeNetworkWithPolicy("172.29.0.0/24", "172.29.0.1", nil, func(string, string, uint16, bool) bool { return true })
	require.NoError(t, err)
	n.egress.activateSource("172.29.0.2")
	free, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	addr := free.Addr().String()
	require.NoError(t, free.Close())
	require.NoError(t, n.Expose("tcp", addr, "172.29.0.2:80"))
	host, err := net.Dial("tcp4", addr)
	require.NoError(t, err)
	defer host.Close()

	n.Close()
	n.Close()
	// The network is gone: its published port is free and the client that
	// was connected to it is not left waiting.
	require.NoError(t, host.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, err = readToEnd(host)
	var ne net.Error
	require.False(t, errors.As(err, &ne) && ne.Timeout(), "a published connection outlived its network")
	ln, err := net.Listen("tcp4", addr)
	require.NoError(t, err, "the published port stayed bound after Close")
	require.NoError(t, ln.Close())
	require.Nil(t, n.egress.reserve("172.29.0.2"))
}
