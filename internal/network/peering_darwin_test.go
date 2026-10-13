package network

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type fakeVMM struct {
	t    *testing.T
	conn *net.UnixConn
}

func dialVMM(t *testing.T, path string, mac [6]byte, features uint64) *fakeVMM {
	t.Helper()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	v := &fakeVMM{t: t, conn: c}
	v.send(peerMessage{Type: peerHello, Value: features, MAC: mac})
	return v
}

func (v *fakeVMM) send(m peerMessage) {
	b := m.encode()
	_, err := v.conn.Write(b[:])
	require.NoError(v.t, err)
}

// read returns the next message and the descriptor attached to it, or -1.
func (v *fakeVMM) read() (peerMessage, int) {
	v.t.Helper()
	var b [peerMessageSize]byte
	oob := make([]byte, unix.CmsgSpace(4))
	used, fd := 0, -1
	require.NoError(v.t, v.conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for used < len(b) {
		n, oobn, _, _, err := v.conn.ReadMsgUnix(b[used:], oob)
		require.NoError(v.t, err)
		if oobn > 0 {
			msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
			require.NoError(v.t, err)
			fds, err := unix.ParseUnixRights(&msgs[0])
			require.NoError(v.t, err)
			fd = fds[0]
		}
		used += n
	}
	m, err := decodePeerMessage(b[:])
	require.NoError(v.t, err)
	return m, fd
}

func (v *fakeVMM) expect(typ uint8) (peerMessage, int) {
	v.t.Helper()
	m, fd := v.read()
	require.Equal(v.t, typ, m.Type, "message %+v", m)
	return m, fd
}

// silent checks that nothing arrives for a short while.
func (v *fakeVMM) silent() {
	v.t.Helper()
	require.NoError(v.t, v.conn.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	var b [1]byte
	_, err := v.conn.Read(b[:])
	require.Error(v.t, err)
}

type peerFixture struct {
	p   *peering
	dir string
	mu  sync.Mutex
	now time.Time
}

func newPeerFixture(t *testing.T) *peerFixture {
	dir, err := os.MkdirTemp("/tmp", "jpeer")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &peerFixture{p: newPeering(), dir: dir, now: time.Unix(1000, 0)}
	f.p.now = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
	return f
}

func (f *peerFixture) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

// member registers a VM and serves its control socket; it returns the socket path.
func (f *peerFixture) member(t *testing.T, last byte) (*peerMember, string) {
	path := filepath.Join(f.dir, "vm"+string('a'+rune(last))+".ctl")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	m := f.p.register(net.HardwareAddr{2, 0, 0, 0, 0, last}, net.IPv4(10, 0, 0, last))
	go f.p.serve(m, ln)
	return m, path
}

func connect(t *testing.T, m *peerMember, path string) (*fakeVMM, uint64) {
	v := dialVMM(t, path, m.mac, peerFeatureDgram)
	w, fd := v.expect(peerWelcome)
	require.Equal(t, -1, fd)
	require.Equal(t, m.mac, w.MAC)
	require.Equal(t, m.ip, w.IP)
	require.NotZero(t, w.Epoch)
	return v, w.Epoch
}

func linked(t *testing.T, a, b int) {
	t.Helper()
	_, err := unix.Write(a, []byte("frame"))
	require.NoError(t, err)
	buf := make([]byte, 16)
	n, err := unix.Read(b, buf)
	require.NoError(t, err)
	require.Equal(t, "frame", string(buf[:n]))
}

func (f *peerFixture) pairs() int {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	return len(f.p.pairs)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, 3*time.Second, 5*time.Millisecond)
}

func TestPeeringPairsOnDemand(t *testing.T) {
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	b, pb := f.member(t, 2)
	va, ea := connect(t, a, pa)
	vb, eb := connect(t, b, pb)
	require.NotEqual(t, ea, eb)

	// A destination that is not a member, or the VM itself, never pairs.
	f.p.observe(a, [6]byte{2, 0, 0, 0, 0, 9})
	f.p.observe(a, a.mac)
	va.silent()

	f.p.observe(a, b.mac)
	ma, fa := va.expect(peerAdd)
	mb, fb := vb.expect(peerAdd)
	require.Equal(t, ea, ma.Epoch)
	require.Equal(t, eb, mb.Epoch)
	require.Equal(t, ma.Value, mb.Value)
	require.Equal(t, b.mac, ma.MAC)
	require.Equal(t, b.ip, ma.IP)
	require.Equal(t, a.mac, mb.MAC)
	linked(t, fa, fb)
	linked(t, fb, fa)
	defer unix.Close(fa)
	defer unix.Close(fb)

	// Repeated observations of a live pair do nothing.
	f.p.observe(b, a.mac)
	vb.silent()

	va.send(peerMessage{Type: peerAck, Flags: 1, Epoch: ea, Value: ma.Value})
	vb.send(peerMessage{Type: peerAck, Flags: 1, Epoch: eb, Value: mb.Value})
	waitFor(t, func() bool {
		f.p.mu.Lock()
		defer f.p.mu.Unlock()
		pr := f.p.byID[ma.Value]
		return pr != nil && pr.acked[0] && pr.acked[1] && pr.held[0] < 0 && pr.held[1] < 0
	})

	// Direct-path statistics reach the link accounting.
	va.send(peerMessage{Type: peerStats, Epoch: 1000, Value: 2000})
	waitFor(t, func() bool { return a.tx.Load() == 1000 && a.rx.Load() == 2000 })

	// A failed link is withdrawn from both sides and retried only after backoff.
	va.send(peerMessage{Type: peerDead, Epoch: ea, Value: ma.Value})
	d, _ := vb.expect(peerDel)
	require.Equal(t, mb.Value, d.Value)
	d, _ = va.expect(peerDel)
	require.Equal(t, ma.Value, d.Value)
	require.Zero(t, f.pairs())
	f.p.observe(a, b.mac)
	va.silent()
	f.advance(peerRetryMin)
	f.p.observe(a, b.mac)
	m2, fd2 := va.expect(peerAdd)
	require.NotEqual(t, ma.Value, m2.Value)
	unix.Close(fd2)
	_, fd2 = vb.expect(peerAdd)
	unix.Close(fd2)

	// A VM that leaves takes its links with it.
	f.p.unregister(b)
	d, _ = va.expect(peerDel)
	require.Equal(t, m2.Value, d.Value)
	require.Zero(t, f.pairs())
	require.Zero(t, a.pairs)
}

func TestPeeringRefusalsAndStaleMessages(t *testing.T) {
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	b, pb := f.member(t, 2)
	va, ea := connect(t, a, pa)
	vb, eb := connect(t, b, pb)

	f.p.observe(a, b.mac)
	ma, fa := va.expect(peerAdd)
	unix.Close(fa)
	_, fb := vb.expect(peerAdd)
	unix.Close(fb)
	// An acknowledgement from an old epoch or for another VM's id is ignored.
	va.send(peerMessage{Type: peerAck, Flags: 0, Epoch: ea + 100, Value: ma.Value})
	vb.send(peerMessage{Type: peerAck, Flags: 0, Epoch: eb, Value: ma.Value + 1})
	va.silent()
	require.Equal(t, 1, f.pairs())
	// A refusal withdraws the pair.
	vb.send(peerMessage{Type: peerAck, Flags: 0, Epoch: eb, Value: ma.Value})
	va.expect(peerDel)
	vb.expect(peerDel)
	require.Zero(t, f.pairs())
}

func TestPeeringAckTimeout(t *testing.T) {
	prev := peerAckWait
	peerAckWait = 50 * time.Millisecond
	t.Cleanup(func() { peerAckWait = prev })
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	b, pb := f.member(t, 2)
	va, ea := connect(t, a, pa)
	vb, _ := connect(t, b, pb)
	f.p.observe(a, b.mac)
	ma, fa := va.expect(peerAdd)
	unix.Close(fa)
	_, fb := vb.expect(peerAdd)
	unix.Close(fb)
	va.send(peerMessage{Type: peerAck, Flags: 1, Epoch: ea, Value: ma.Value})
	// b never answers: the pair is withdrawn from both.
	va.expect(peerDel)
	vb.expect(peerDel)
	require.Zero(t, f.pairs())
}

func TestPeeringSessionReplacementAndHello(t *testing.T) {
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	b, pb := f.member(t, 2)

	// Wrong MAC: closed without a welcome.
	bad := dialVMM(t, pa, b.mac, peerFeatureDgram)
	require.NoError(t, bad.conn.SetReadDeadline(time.Now().Add(time.Second)))
	var one [1]byte
	_, err := bad.conn.Read(one[:])
	require.Error(t, err)

	// No datagram feature: the connection stays open but nothing is offered.
	plain := dialVMM(t, pb, b.mac, 0)
	plain.silent()
	require.Nil(t, b.session)
	plain.conn.Close()

	va, ea := connect(t, a, pa)
	vb, _ := connect(t, b, pb)
	f.p.observe(a, b.mac)
	_, fa := va.expect(peerAdd)
	unix.Close(fa)
	_, fb := vb.expect(peerAdd)
	unix.Close(fb)

	// The broker reconnects (switch-side view of a VMM reset): new epoch,
	// and the partner loses the old link.
	va2, ea2 := connect(t, a, pa)
	require.Greater(t, ea2, ea)
	vb.expect(peerDel)
	require.Zero(t, f.pairs())
	_ = va2
}

func TestPeeringIdleReclaimsWithoutBackoff(t *testing.T) {
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	b, pb := f.member(t, 2)
	va, ea := connect(t, a, pa)
	vb, _ := connect(t, b, pb)
	f.p.observe(a, b.mac)
	ma, fa := va.expect(peerAdd)
	unix.Close(fa)
	_, fb := vb.expect(peerAdd)
	unix.Close(fb)
	va.send(peerMessage{Type: peerIdle, Epoch: ea, Value: ma.Value})
	va.expect(peerDel)
	vb.expect(peerDel)
	require.Zero(t, f.pairs())
	require.Zero(t, a.pairs)
	// Not a failure: traffic that resumes pairs them again at once.
	f.p.observe(a, b.mac)
	_, fa = va.expect(peerAdd)
	unix.Close(fa)
}

func TestPeeringCap(t *testing.T) {
	f := newPeerFixture(t)
	a, pa := f.member(t, 1)
	b, pb := f.member(t, 2)
	va, _ := connect(t, a, pa)
	connect(t, b, pb)
	f.p.mu.Lock()
	a.pairs = peerMaxPerVM
	f.p.mu.Unlock()
	f.p.observe(a, b.mac)
	va.silent()
	require.Zero(t, f.pairs())
}

func TestEthernetConnObservesUnicastFlows(t *testing.T) {
	var seen [][6]byte
	e := &ethernetConn{observe: func(dst [6]byte) { seen = append(seen, dst) }}
	unicast := make([]byte, 60)
	copy(unicast, []byte{2, 0, 0, 0, 0, 2})
	broadcast := make([]byte, 60)
	copy(broadcast, []byte{255, 255, 255, 255, 255, 255})
	for i := 0; i < peerPromoteFrames-1; i++ {
		e.count(unicast)
		e.count(broadcast)
	}
	require.Empty(t, seen)
	e.count(unicast)
	require.Equal(t, [][6]byte{{2, 0, 0, 0, 0, 2}}, seen)
	// Counting restarts, so a refused promotion is asked again later.
	for i := 0; i < peerPromoteFrames; i++ {
		e.count(unicast)
	}
	require.Len(t, seen, 2)
}
