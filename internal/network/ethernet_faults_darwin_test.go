package network

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// A VM's Ethernet link carries whatever the guest and its VMM send, and both
// can be hostile or simply die. The switch port must only ever pass on whole
// frames that carry the VM's own addresses, end the link on anything it
// cannot frame, and never be stopped by one link that stalls or vanishes.

const (
	portIP  = "172.20.0.2"
	portMAC = "02:01:02:03:04:05"
)

// streamConn is the VMM end of a link as a fixed byte stream: reads return
// the stream and then EOF (the VMM is gone), writes go nowhere.
type streamConn struct {
	mu      sync.Mutex
	r       *bytes.Reader
	wrote   atomic.Int64
	closed  atomic.Bool
	failing error // returned by Write when set
}

func newStreamConn(stream []byte) *streamConn { return &streamConn{r: bytes.NewReader(stream)} }

func (s *streamConn) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.Read(p)
}

func (s *streamConn) Write(p []byte) (int, error) {
	if s.failing != nil {
		return 0, s.failing
	}
	s.wrote.Add(int64(len(p)))
	return len(p), nil
}
func (s *streamConn) Close() error                     { s.closed.Store(true); return nil }
func (s *streamConn) LocalAddr() net.Addr              { return nil }
func (s *streamConn) RemoteAddr() net.Addr             { return nil }
func (s *streamConn) SetDeadline(time.Time) error      { return nil }
func (s *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (s *streamConn) SetWriteDeadline(time.Time) error { return nil }

// guestFrame is a minimal IPv4 frame from the port's own VM, length-prefixed
// as on the wire.
func guestFrame(size int, mutate func(eth []byte)) []byte {
	f := make([]byte, 4+size)
	binary.BigEndian.PutUint32(f, uint32(size))
	eth := f[4:]
	hw, _ := net.ParseMAC(portMAC)
	copy(eth[0:6], []byte{0x02, 9, 9, 9, 9, 9})
	copy(eth[6:12], hw)
	binary.BigEndian.PutUint16(eth[12:14], 0x0800)
	if size > 14 {
		eth[14] = 0x45
	}
	if size >= 30 {
		copy(eth[26:30], net.ParseIP(portIP).To4())
	}
	if mutate != nil {
		mutate(eth)
	}
	return f
}

// readFrames reads the port until it fails and returns what it delivered,
// one element per Read.
func readFrames(e *ethernetConn) (frames [][]byte, err error) {
	buf := make([]byte, maxEthernetFrame+4)
	for {
		n, rerr := e.Read(buf)
		if rerr != nil {
			return frames, rerr
		}
		frames = append(frames, append([]byte(nil), buf[:n]...))
	}
}

func TestEthernetFaults_VMMDiesAtEveryByteOfAFrame(t *testing.T) {
	first, second := guestFrame(60, nil), guestFrame(200, func(eth []byte) { eth[40] = 0x7f })
	wire := append(append([]byte(nil), first...), second...)
	for cut := range len(wire) {
		conn := newStreamConn(wire[:cut])
		e := newEthernetConn(conn, portIP, portMAC)
		frames, err := readFrames(e)
		require.Error(t, err, "cut at %d", cut)
		_ = e.Close()

		// The announcement, then only frames that arrived whole: a frame
		// the VMM did not finish is never delivered short or padded.
		want := [][]byte{e.announcement()}
		if cut >= len(first) {
			want = append(want, first)
		}
		require.Equal(t, want, frames, "cut at %d", cut)
		// Where the cut falls relative to the frame it is in: between frames
		// the link ended cleanly; inside a frame's bytes it did not.
		within := cut
		if cut >= len(first) {
			within = cut - len(first)
		}
		switch {
		case within == 0:
			require.ErrorIs(t, err, io.EOF, "cut at %d", cut)
		case within > 4:
			require.ErrorIs(t, err, io.ErrUnexpectedEOF, "cut at %d: a link closed inside a frame is not a clean end", cut)
		}
	}
}

func TestEthernetFaults_UnframeableLengthEndsTheLink(t *testing.T) {
	good := guestFrame(60, nil)
	for _, size := range []uint32{0, 1, 13, maxEthernetFrame + 1, 1 << 24, 0x7fffffff, 0x80000000, 0xffffffff} {
		var bad [4]byte
		binary.BigEndian.PutUint32(bad[:], size)
		// A valid frame, a length no frame has, and more valid frames after.
		wire := bytes.Join([][]byte{good, bad[:], good, good}, nil)
		e := newEthernetConn(newStreamConn(wire), portIP, portMAC)
		frames, err := readFrames(e)
		_ = e.Close()
		require.ErrorContains(t, err, "invalid Ethernet frame length", "length %d", size)
		// Once the framing is lost nothing after it can be trusted: the
		// bytes that follow are not searched for something frame-shaped.
		require.Equal(t, [][]byte{e.announcement(), good}, frames, "length %d", size)
	}
}

func TestEthernetFaults_ForgedFramesAreDroppedNotDelivered(t *testing.T) {
	other := net.ParseIP("172.20.0.9").To4()
	arp := func(mutate func(eth []byte)) []byte {
		return guestFrame(60, func(eth []byte) {
			hw, _ := net.ParseMAC(portMAC)
			for i := 14; i < len(eth); i++ {
				eth[i] = 0
			}
			binary.BigEndian.PutUint16(eth[12:14], 0x0806)
			binary.BigEndian.PutUint16(eth[14:16], 1)
			binary.BigEndian.PutUint16(eth[16:18], 0x0800)
			eth[18], eth[19] = 6, 4
			binary.BigEndian.PutUint16(eth[20:22], 2)
			copy(eth[22:28], hw)
			copy(eth[28:32], net.ParseIP(portIP).To4())
			if mutate != nil {
				mutate(eth)
			}
		})
	}
	forged := map[string][]byte{
		"another VM's MAC":         guestFrame(60, func(eth []byte) { eth[11] ^= 1 }),
		"broadcast source MAC":     guestFrame(60, func(eth []byte) { copy(eth[6:12], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) }),
		"the gateway's MAC":        guestFrame(60, func(eth []byte) { copy(eth[6:12], gatewayMAC) }),
		"another VM's IP":          guestFrame(60, func(eth []byte) { copy(eth[26:30], other) }),
		"unspecified source IP":    guestFrame(60, func(eth []byte) { copy(eth[26:30], []byte{0, 0, 0, 0}) }),
		"IPv6":                     guestFrame(60, func(eth []byte) { binary.BigEndian.PutUint16(eth[12:14], 0x86dd) }),
		"VLAN tag":                 guestFrame(60, func(eth []byte) { binary.BigEndian.PutUint16(eth[12:14], 0x8100) }),
		"LLC length field":         guestFrame(60, func(eth []byte) { binary.BigEndian.PutUint16(eth[12:14], 46) }),
		"IP version 6 in IPv4":     guestFrame(60, func(eth []byte) { eth[14] = 0x65 }),
		"IP header length zero":    guestFrame(60, func(eth []byte) { eth[14] = 0x40 }),
		"IP header length four":    guestFrame(60, func(eth []byte) { eth[14] = 0x44 }),
		"IP header past the frame": guestFrame(40, func(eth []byte) { eth[14] = 0x4f }),
		"IPv4 too short":           guestFrame(33, nil),
		"header only":              guestFrame(14, nil),
		"ARP for another IP":       arp(func(eth []byte) { copy(eth[28:32], other) }),
		"ARP for another MAC":      arp(func(eth []byte) { eth[27] ^= 1 }),
		"ARP not Ethernet":         arp(func(eth []byte) { eth[15] = 6 }),
		"ARP not IPv4":             arp(func(eth []byte) { binary.BigEndian.PutUint16(eth[16:18], 0x86dd) }),
		"ARP odd address sizes":    arp(func(eth []byte) { eth[18], eth[19] = 8, 16 }),
		"ARP cut short":            arp(nil)[:4+41],
		"capabilities, bad magic":  guestFrame(60, func(eth []byte) { binary.BigEndian.PutUint16(eth[12:14], switchCapsType) }),
		"oversized, not to gateway": guestFrame(20000, func(eth []byte) {
			eth[23] = 6
			binary.BigEndian.PutUint16(eth[16:18], 20000-14)
		}),
		"oversized to gateway, not TCP": guestFrame(20000, func(eth []byte) {
			copy(eth[0:6], gatewayMAC)
			eth[23] = 17
			binary.BigEndian.PutUint16(eth[16:18], 20000-14)
		}),
		"oversized to gateway, length lies": guestFrame(20000, func(eth []byte) {
			copy(eth[0:6], gatewayMAC)
			eth[23] = 6
			binary.BigEndian.PutUint16(eth[16:18], 1500)
		}),
	}
	// A cut ARP frame needs its length prefix to say what is really there.
	binary.BigEndian.PutUint32(forged["ARP cut short"], 41)

	good := guestFrame(60, func(eth []byte) { eth[50] = 0x42 })
	goodARP := arp(nil)
	probe := arp(func(eth []byte) { copy(eth[28:32], []byte{0, 0, 0, 0}) }) // address probe, sender 0.0.0.0
	for name, frame := range forged {
		t.Run(name, func(t *testing.T) {
			wire := bytes.Join([][]byte{frame, good, frame, frame, goodARP, probe, frame}, nil)
			e := newEthernetConn(newStreamConn(wire), portIP, portMAC)
			observed := 0
			e.capabilities = func(byte) { observed++ }
			frames, err := readFrames(e)
			_ = e.Close()
			require.ErrorIs(t, err, io.EOF)
			// The forged frame never reaches the switch, and dropping it
			// does not disturb the frames around it.
			require.Equal(t, [][]byte{e.announcement(), good, goodARP, probe}, frames)
			require.Zero(t, observed, "a forged frame changed what the port believes the VMM supports")
		})
	}
}

func TestEthernetFaults_CapabilitiesOnlyFromThePortsOwnVMM(t *testing.T) {
	caps := func(mutate func(eth []byte)) []byte {
		return guestFrame(switchCapsSize, func(eth []byte) {
			for i := 14; i < len(eth); i++ {
				eth[i] = 0
			}
			binary.BigEndian.PutUint16(eth[12:14], switchCapsType)
			copy(eth[14:18], switchCapsMagic)
			eth[18], eth[19] = 1, switchCapsLarge
			if mutate != nil {
				mutate(eth)
			}
		})
	}
	for name, tc := range map[string]struct {
		frame []byte
		want  []byte
	}{
		"genuine":                {caps(nil), []byte{switchCapsLarge}},
		"genuine, flags cleared": {caps(func(eth []byte) { eth[19] = 0 }), []byte{0}},
		"unknown future flags":   {caps(func(eth []byte) { eth[19] = 0xff }), []byte{0xff}},
		"from another MAC":       {caps(func(eth []byte) { eth[6] ^= 4 }), nil},
		"another version":        {caps(func(eth []byte) { eth[18] = 2 }), nil},
		"wrong magic":            {caps(func(eth []byte) { eth[14] = 'X' }), nil},
		"cut before the flags": {func() []byte {
			f := caps(nil)[:4+19]
			binary.BigEndian.PutUint32(f, 19)
			return f
		}(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			good := guestFrame(60, nil)
			e := newEthernetConn(newStreamConn(append(append([]byte(nil), tc.frame...), good...)), portIP, portMAC)
			var got []byte
			e.capabilities = func(flags byte) { got = append(got, flags) }
			frames, err := readFrames(e)
			_ = e.Close()
			require.ErrorIs(t, err, io.EOF)
			require.Equal(t, tc.want, got)
			// Capability frames are link-local: none is ever switched on
			// to other VMs, genuine or not.
			require.Equal(t, [][]byte{e.announcement(), good}, frames)
		})
	}
}

func TestEthernetFaults_MalformedOutgoingFrameIsRefused(t *testing.T) {
	conn := newStreamConn(nil)
	e := newEthernetConn(conn, portIP, portMAC)
	defer e.Close()
	valid := guestFrame(60, nil)
	lying := append([]byte(nil), valid...)
	binary.BigEndian.PutUint32(lying, 61)
	short := append([]byte(nil), valid...)
	binary.BigEndian.PutUint32(short, 59)
	huge := make([]byte, maxEthernetFrame+5)
	binary.BigEndian.PutUint32(huge, maxEthernetFrame+1)

	queuedBefore := func() int64 { return e.queuedBytes.Load() }
	for name, b := range map[string][]byte{
		"nil": nil, "empty": {}, "prefix only": valid[:4], "below an Ethernet header": valid[:17],
		"length says more": lying, "length says less": short, "above the frame limit": huge,
		"two frames in one write": append(append([]byte(nil), valid...), valid...),
	} {
		before := queuedBefore()
		n, err := e.Write(b)
		require.ErrorContains(t, err, "invalid outgoing frame", name)
		require.Zero(t, n, name)
		// A refused frame must not eat into the queue budget, or enough of
		// them would stall the port.
		require.LessOrEqual(t, e.queuedBytes.Load(), before, name)
	}
	// The port still works.
	n, err := e.Write(valid)
	require.NoError(t, err)
	require.Equal(t, len(valid), n)
	want := int64(len(e.switchCapabilities()) + len(valid))
	require.Eventually(t, func() bool { return conn.wrote.Load() == want }, 10*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return e.queuedBytes.Load() == 0 }, 10*time.Second, time.Millisecond, "delivered frames stayed counted against the queue")
}

func TestEthernetFaults_CloseIsFinalAndRunsItsHookOnce(t *testing.T) {
	conn := newStreamConn(nil)
	e := newEthernetConn(conn, portIP, portMAC)
	var hooks atomic.Int64
	e.closed = func() { hooks.Add(1) }

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.Close()
		}()
	}
	// Writers racing the close get a definite answer either way.
	valid := guestFrame(60, nil)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Write(valid); err != nil {
				assert.ErrorIs(t, err, net.ErrClosed)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, hooks.Load(), "the close hook withdraws the port's state exactly once")
	require.True(t, conn.closed.Load())
	_, err := e.Write(valid)
	require.ErrorIs(t, err, net.ErrClosed)
	require.NoError(t, e.Close(), "closing a closed link is not an error")
	require.EqualValues(t, 1, hooks.Load())
}

func TestEthernetFaults_VMMGoneOnWriteClosesThePort(t *testing.T) {
	conn := newStreamConn(nil)
	conn.failing = errors.New("write: broken pipe")
	e := newEthernetConn(conn, portIP, portMAC)
	// The first write to the VMM (the frame that opens the link) fails: the
	// port closes itself, so the switch stops queueing frames for a dead VM.
	select {
	case <-e.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the port stayed open after its VMM was gone")
	}
	require.Eventually(t, func() bool {
		_, err := e.Write(guestFrame(60, nil))
		return errors.Is(err, net.ErrClosed)
	}, 10*time.Second, time.Millisecond)
	require.True(t, conn.closed.Load())
}

func TestFrameCounter_CountsPayloadHoweverTheStreamIsChunked(t *testing.T) {
	sizes := []int{14, 60, 1, 0, 9000, 65536, 0, 0, 33}
	var stream []byte
	total := int64(0)
	for i, size := range sizes {
		var h [4]byte
		binary.BigEndian.PutUint32(h[:], uint32(size))
		stream = append(stream, h[:]...)
		stream = append(stream, bytes.Repeat([]byte{byte(i + 1)}, size)...)
		total += int64(size)
	}
	for _, chunk := range []int{1, 2, 3, 4, 5, 7, 4096, len(stream)} {
		var c frameCounter
		var got int64
		for off := 0; off < len(stream); off += chunk {
			got += c.count(stream[off:min(off+chunk, len(stream))])
		}
		require.Equal(t, total, got, "chunks of %d bytes", chunk)
		require.Zero(t, c.remaining)
		require.Zero(t, c.used)
	}
	var c frameCounter
	require.Zero(t, c.count(nil))
	// A stream cut inside a frame counts what was seen, not what was promised.
	require.Equal(t, int64(10), c.count(append([]byte{0, 0, 0, 60}, make([]byte, 10)...)))
}

func FuzzFrameCounter(f *testing.F) {
	f.Add([]byte{0, 0, 0, 2, 9, 9, 0, 0, 0, 0, 0, 0, 0, 1, 7}, uint8(3))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 1, 2, 3}, uint8(1))
	f.Add([]byte{0, 0}, uint8(0))
	f.Fuzz(func(t *testing.T, stream []byte, chunk uint8) {
		var whole, split frameCounter
		want := whole.count(stream)
		if want < 0 || want > int64(len(stream)) {
			t.Fatalf("%d payload bytes counted in a %d-byte stream", want, len(stream))
		}
		step := int(chunk)%17 + 1
		var got int64
		for off := 0; off < len(stream); off += step {
			got += split.count(stream[off:min(off+step, len(stream))])
		}
		// Traffic statistics must not depend on how the kernel happened to
		// split the stream into reads.
		if got != want || split != whole {
			t.Fatalf("chunks of %d: counted %d, whole stream %d", step, got, want)
		}
	})
}

func FuzzEthernetRead(f *testing.F) {
	good := guestFrame(60, nil)
	f.Add(good)
	f.Add(append(append([]byte(nil), good...), good[:30]...))
	f.Add(guestFrame(60, func(eth []byte) { eth[11] ^= 1 }))
	f.Add(guestFrame(60, func(eth []byte) { binary.BigEndian.PutUint16(eth[12:14], 0x0806) }))
	f.Add(guestFrame(60, func(eth []byte) {
		binary.BigEndian.PutUint16(eth[12:14], switchCapsType)
		copy(eth[14:18], switchCapsMagic)
		eth[18] = 1
	}))
	f.Add([]byte{0, 0, 0, 14})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{})
	hw, _ := net.ParseMAC(portMAC)
	ip := net.ParseIP(portIP).To4()
	f.Fuzz(func(t *testing.T, stream []byte) {
		e := newEthernetConn(newStreamConn(stream), portIP, portMAC)
		defer e.Close()
		frames, err := readFrames(e)
		if err == nil {
			t.Fatal("a finite stream never ended")
		}
		if len(frames) == 0 || !bytes.Equal(frames[0], e.announcement()) {
			t.Fatal("the port did not announce its VM first")
		}
		delivered := 0
		for _, fr := range frames[1:] {
			// Everything the switch is given is one whole frame from this
			// port's VM: its MAC, and its IP wherever the frame carries one.
			if len(fr) < 4+14 || int(binary.BigEndian.Uint32(fr[:4])) != len(fr)-4 {
				t.Fatalf("a frame of %d bytes was delivered with length prefix %x", len(fr), fr[:min(4, len(fr))])
			}
			eth := fr[4:]
			if !bytes.Equal(eth[6:12], hw) {
				t.Fatalf("frame delivered with source MAC %x", eth[6:12])
			}
			switch binary.BigEndian.Uint16(eth[12:14]) {
			case 0x0800:
				if len(eth) < 34 || !bytes.Equal(eth[26:30], ip) {
					t.Fatalf("IPv4 frame delivered with a foreign source: %x", eth[:min(34, len(eth))])
				}
			case 0x0806:
				if len(eth) < 42 || !bytes.Equal(eth[22:28], hw) || (!bytes.Equal(eth[28:32], ip) && !bytes.Equal(eth[28:32], []byte{0, 0, 0, 0})) {
					t.Fatalf("ARP frame delivered claiming another sender: %x", eth[:min(42, len(eth))])
				}
			default:
				t.Fatalf("frame of EtherType %#04x delivered", binary.BigEndian.Uint16(eth[12:14]))
			}
			delivered += len(fr)
		}
		if delivered > len(stream) {
			t.Fatalf("%d bytes delivered from a %d-byte stream", delivered, len(stream))
		}
	})
}

func FuzzFrameValidators(f *testing.F) {
	f.Add(guestFrame(60, nil)[4:])
	f.Add(guestFrame(14, nil)[4:])
	f.Add(guestFrame(20000, func(eth []byte) { copy(eth[0:6], gatewayMAC); eth[23] = 6 })[4:])
	f.Add([]byte{})
	f.Add(make([]byte, 13))
	e := &ethernetConn{}
	e.mac, _ = net.ParseMAC(portMAC)
	e.ip = net.ParseIP(portIP).To4()
	f.Fuzz(func(t *testing.T, frame []byte) {
		// None of the checks may read past the frame, whatever its length.
		source := e.validSource(frame)
		large := validLarge(frame)
		_, ours, isCaps := e.capabilitiesFrame(frame)
		if ours && !isCaps {
			t.Fatal("a frame is the VMM's capabilities answer without being a capabilities frame")
		}
		if source && len(frame) < 34 {
			t.Fatalf("a %d-byte frame passed source validation", len(frame))
		}
		if source && isCaps {
			t.Fatal("a capabilities frame passed as guest traffic")
		}
		if len(frame) > 14+9000 && large && !bytes.Equal(frame[:6], gatewayMAC) {
			t.Fatal("a frame above the MTU was accepted for a destination other than the gateway")
		}
	})
}

func FuzzSegmentTCPv4(f *testing.F) {
	f.Add(tcpPacket(3000, header.TCPFlagAck|header.TCPFlagPsh|header.TCPFlagFin), 1448)
	f.Add(tcpPacket(1, header.TCPFlagAck), 1)
	f.Add(tcpPacket(0, header.TCPFlagSyn), 1448)
	f.Add(tcpPacket(100, header.TCPFlagAck)[:45], 50)
	f.Add([]byte{0x45, 0, 0, 20}, 100)
	f.Add([]byte{}, 0)
	f.Add(tcpPacket(64, header.TCPFlagAck), -1)
	f.Fuzz(func(t *testing.T, pkt []byte, mss int) {
		if len(pkt) > 65535 || (mss > 0 && len(pkt)/mss > 4096) {
			return
		}
		orig := append([]byte(nil), pkt...)
		segs := segmentTCPv4(pkt, mss)
		if !bytes.Equal(orig, pkt) {
			t.Fatal("the packet was modified in place")
		}
		if segs == nil {
			return
		}
		ihl := int(header.IPv4(pkt).HeaderLength())
		headers := ihl + int(header.TCP(pkt[ihl:]).DataOffset())
		seq := header.TCP(pkt[ihl:]).SequenceNumber()
		var payload []byte
		for i, seg := range segs {
			if len(seg) <= headers || len(seg)-headers > mss {
				t.Fatalf("segment %d carries %d payload bytes with MSS %d", i, len(seg)-headers, mss)
			}
			ip := header.IPv4(seg)
			if int(ip.TotalLength()) != len(seg) || !ip.IsChecksumValid() {
				t.Fatalf("segment %d: IPv4 header is not consistent", i)
			}
			tcp := header.TCP(seg[ihl:])
			if tcp.SequenceNumber() != seq+uint32(len(payload)) {
				t.Fatalf("segment %d: sequence %d, want %d", i, tcp.SequenceNumber(), seq+uint32(len(payload)))
			}
			pseudo := header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(seg)-ihl))
			if checksum.Checksum(seg[ihl:], pseudo) != 0xffff {
				t.Fatalf("segment %d: TCP checksum does not verify", i)
			}
			if i < len(segs)-1 && tcp.Flags()&(header.TCPFlagFin|header.TCPFlagPsh) != 0 {
				t.Fatalf("segment %d of %d ends the stream early", i, len(segs))
			}
			payload = append(payload, seg[headers:]...)
		}
		// The guest receives exactly the bytes the gateway sent, in order.
		if !bytes.Equal(payload, pkt[headers:]) {
			t.Fatalf("%d payload bytes became %d", len(pkt)-headers, len(payload))
		}
	})
}
