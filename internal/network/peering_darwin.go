package network

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Direct VM-to-VM links (see peerproto.go). The switch keeps every frame path
// it has today; when two members of one network exchange enough unicast
// frames through it, it gives both VMMs the ends of a datagram socketpair and
// their unicast frames stop crossing the broker and switch hops. Pairs are
// created on demand, so the cost is per active pair, never per member pair.
const (
	peerPromoteFrames = 32 // switched unicast frames to one destination before pairing
	peerMaxPerVM      = 64 // the VMM's peer table size
	peerHelloWait     = 5 * time.Second
	peerWriteWait     = time.Second
	peerRetryMin      = time.Second
	peerRetryMax      = time.Minute
	peerOutQueue      = 128
)

// peerAckWait bounds how long a new pair may wait for both VMMs to accept it.
var peerAckWait = 5 * time.Second

type pairKey [12]byte

func makePairKey(a, b [6]byte) pairKey {
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}
	var k pairKey
	copy(k[:6], a[:])
	copy(k[6:], b[:])
	return k
}

type peering struct {
	mu      sync.Mutex
	members map[[6]byte]*peerMember
	pairs   map[pairKey]*peerPair
	byID    map[uint64]*peerPair
	retry   map[pairKey]peerBackoff
	epoch   uint64
	nextID  uint32
	now     func() time.Time
}

type peerBackoff struct {
	until time.Time
	delay time.Duration
}

type peerMember struct {
	mac     [6]byte
	ip      [4]byte
	session *peerSession
	pairs   int
	// Cumulative bytes this VM sent and received over direct links, as
	// reported by its VMM; the switch never sees those frames.
	tx, rx atomic.Uint64
}

type peerOut struct {
	msg peerMessage
	fd  int // owned by the writer and closed after the write; -1 for none
}

type peerSession struct {
	member *peerMember
	conn   *net.UnixConn
	epoch  uint64
	out    chan peerOut
	done   chan struct{}
	closed bool // guarded by peering.mu
}

type peerPair struct {
	id    uint64
	key   pairKey
	side  [2]*peerSession
	held  [2]int // the switch's reference to each end until that VMM acknowledges it
	acked [2]bool
}

func newPeering() *peering {
	return &peering{
		members: map[[6]byte]*peerMember{},
		pairs:   map[pairKey]*peerPair{},
		byID:    map[uint64]*peerPair{},
		retry:   map[pairKey]peerBackoff{},
		now:     time.Now,
	}
}

func (p *peering) register(mac net.HardwareAddr, ip net.IP) *peerMember {
	m := &peerMember{}
	copy(m.mac[:], mac)
	copy(m.ip[:], ip.To4())
	p.mu.Lock()
	defer p.mu.Unlock()
	if old := p.members[m.mac]; old != nil {
		p.endSessionLocked(old.session)
	}
	p.members[m.mac] = m
	return m
}

func (p *peering) unregister(m *peerMember) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endSessionLocked(m.session)
	if p.members[m.mac] == m {
		delete(p.members, m.mac)
	}
	for k := range p.retry {
		if bytes.Equal(k[:6], m.mac[:]) || bytes.Equal(k[6:], m.mac[:]) {
			delete(p.retry, k)
		}
	}
}

// serve accepts the member's control connections (one live session; a new
// connection from the same VMM replaces the old one) until ln is closed.
func (p *peering) serve(m *peerMember, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.session(m, c.(*net.UnixConn))
	}
}

func (p *peering) session(m *peerMember, c *net.UnixConn) {
	var b [peerMessageSize]byte
	_ = c.SetReadDeadline(time.Now().Add(peerHelloWait))
	if _, err := io.ReadFull(c, b[:]); err != nil {
		c.Close()
		return
	}
	hello, err := decodePeerMessage(b[:])
	if err != nil || hello.Type != peerHello || hello.MAC != m.mac {
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if hello.Value&peerFeatureDgram == 0 {
		// Nothing this switch can offer; keep the connection so the VMM
		// does not reconnect in a loop.
		_, _ = io.Copy(io.Discard, c)
		c.Close()
		return
	}
	s := &peerSession{member: m, conn: c, out: make(chan peerOut, peerOutQueue), done: make(chan struct{})}
	p.mu.Lock()
	if p.members[m.mac] != m {
		p.mu.Unlock()
		c.Close()
		return
	}
	p.endSessionLocked(m.session)
	p.epoch++
	s.epoch = p.epoch
	m.session = s
	p.enqueueLocked(s, peerMessage{Type: peerWelcome, Epoch: s.epoch, MAC: m.mac, IP: m.ip}, -1)
	p.mu.Unlock()
	go p.writer(s)
	for {
		if _, err := io.ReadFull(c, b[:]); err != nil {
			break
		}
		msg, err := decodePeerMessage(b[:])
		if err != nil {
			break
		}
		switch msg.Type {
		case peerAck:
			p.acked(s, msg)
		case peerDead:
			p.dead(s, msg)
		case peerIdle:
			p.idle(s, msg)
		case peerStats:
			m.tx.Store(msg.Epoch)
			m.rx.Store(msg.Value)
		default:
			slog.Debug("native peering: unexpected control message", "type", msg.Type)
		}
	}
	p.mu.Lock()
	p.endSessionLocked(s)
	p.mu.Unlock()
}

func (p *peering) writer(s *peerSession) {
	defer func() {
		for {
			select {
			case o := <-s.out:
				closeFD(o.fd)
			default:
				return
			}
		}
	}()
	for {
		select {
		case <-s.done:
			return
		case o := <-s.out:
			b := o.msg.encode()
			var oob []byte
			if o.fd >= 0 {
				oob = unix.UnixRights(o.fd)
			}
			_ = s.conn.SetWriteDeadline(time.Now().Add(peerWriteWait))
			n, oobn, err := s.conn.WriteMsgUnix(b[:], oob, nil)
			closeFD(o.fd)
			if err != nil || n != len(b) || oobn != len(oob) {
				p.mu.Lock()
				p.endSessionLocked(s)
				p.mu.Unlock()
				return
			}
		}
	}
}

// enqueueLocked never blocks the caller; a session that cannot keep up with
// its control queue is ended, which withdraws every link it holds.
func (p *peering) enqueueLocked(s *peerSession, msg peerMessage, fd int) {
	if s.closed {
		closeFD(fd)
		return
	}
	select {
	case s.out <- peerOut{msg: msg, fd: fd}:
	default:
		closeFD(fd)
		p.endSessionLocked(s)
	}
}

func (p *peering) endSessionLocked(s *peerSession) {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	close(s.done)
	_ = s.conn.Close()
	if s.member.session == s {
		s.member.session = nil
	}
	for _, pr := range p.pairs {
		if pr.side[0] == s || pr.side[1] == s {
			p.removePairLocked(pr, false)
		}
	}
}

func (p *peering) removePairLocked(pr *peerPair, failed bool) {
	if p.pairs[pr.key] != pr {
		return
	}
	delete(p.pairs, pr.key)
	delete(p.byID, pr.id)
	for i, s := range pr.side {
		closeFD(pr.held[i])
		pr.held[i] = -1
		s.member.pairs--
		p.enqueueLocked(s, peerMessage{Type: peerDel, Epoch: s.epoch, Value: pr.id}, -1)
	}
	if failed {
		b := p.retry[pr.key]
		b.delay = min(max(2*b.delay, peerRetryMin), peerRetryMax)
		b.until = p.now().Add(b.delay)
		p.retry[pr.key] = b
	}
}

// observe is called by a member's switch port when it keeps sending unicast
// frames to dst. It pairs the two VMMs when both have a live session.
func (p *peering) observe(src *peerMember, dst [6]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.members[dst]
	if d == nil || d == src || src.session == nil || d.session == nil ||
		src.pairs >= peerMaxPerVM || d.pairs >= peerMaxPerVM {
		return
	}
	key := makePairKey(src.mac, dst)
	if p.pairs[key] != nil || p.now().Before(p.retry[key].until) {
		return
	}
	fds, err := socketpair()
	if err != nil {
		slog.Warn("native peering: socketpair", "err", err)
		return
	}
	p.nextID++
	if p.nextID == 0 {
		p.nextID++
	}
	pr := &peerPair{id: uint64(p.nextID), key: key, side: [2]*peerSession{src.session, d.session}, held: fds}
	p.pairs[key] = pr
	p.byID[pr.id] = pr
	src.pairs++
	d.pairs++
	for i, s := range pr.side {
		other := pr.side[1-i].member
		dup, err := dupFD(fds[i])
		if err != nil {
			p.removePairLocked(pr, true)
			return
		}
		p.enqueueLocked(s, peerMessage{Type: peerAdd, Epoch: s.epoch, Value: pr.id, MAC: other.mac, IP: other.ip}, dup)
	}
	time.AfterFunc(peerAckWait, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.byID[pr.id] == pr && !(pr.acked[0] && pr.acked[1]) {
			p.removePairLocked(pr, true)
		}
	})
}

func (p *peering) find(s *peerSession, msg peerMessage) (*peerPair, int) {
	pr := p.byID[msg.Value]
	if pr == nil || msg.Epoch != s.epoch {
		return nil, -1
	}
	for i, side := range pr.side {
		if side == s {
			return pr, i
		}
	}
	return nil, -1
}

func (p *peering) acked(s *peerSession, msg peerMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, i := p.find(s, msg)
	if pr == nil {
		return
	}
	if msg.Flags != 1 {
		p.removePairLocked(pr, true)
		return
	}
	closeFD(pr.held[i])
	pr.held[i] = -1
	pr.acked[i] = true
}

func (p *peering) dead(s *peerSession, msg peerMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr, _ := p.find(s, msg); pr != nil {
		p.removePairLocked(pr, true)
	}
}

// idle withdraws a link that carried no frames for a while, so idle pairs do
// not hold the per-VM budget; traffic that resumes pairs the VMs again.
func (p *peering) idle(s *peerSession, msg peerMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr, _ := p.find(s, msg); pr != nil {
		p.removePairLocked(pr, false)
	}
}

func socketpair() ([2]int, error) {
	// Hold ForkLock so a concurrent VMM launch cannot inherit these.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return [2]int{-1, -1}, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return [2]int{fds[0], fds[1]}, nil
}

func dupFD(fd int) (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	d, err := unix.Dup(fd)
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(d)
	return d, nil
}

func closeFD(fd int) {
	if fd >= 0 {
		_ = unix.Close(fd)
	}
}
