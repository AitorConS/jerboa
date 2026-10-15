package network

import (
	"encoding/binary"
	"sync"

	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// gsoMaxSize bounds the gateway's TCP segments. With Ethernet framing they
// stay within the VMM's 65526-byte frame limit.
const gsoMaxSize = 65000

// gsoEndpoint lets the gateway's TCP build segments of up to gsoMaxSize
// instead of one per MTU: each crosses the switch and the VMM link once.
// Segments that fit the MTU leave with complete checksums. Larger ones go
// whole, with the partial checksum, only to ports whose VMM said it takes
// them (switch capabilities); every other port receives MTU-sized segments.
type gsoEndpoint struct {
	*tap.LinkEndpoint
	mu    sync.RWMutex
	large map[tcpip.LinkAddress]*ethernetConn
}

func newGSOEndpoint(ep *tap.LinkEndpoint) *gsoEndpoint {
	return &gsoEndpoint{LinkEndpoint: ep, large: map[tcpip.LinkAddress]*ethernetConn{}}
}

func (e *gsoEndpoint) GSOMaxSize() uint32               { return gsoMaxSize }
func (e *gsoEndpoint) SupportedGSO() stack.SupportedGSO { return stack.HostGSOSupported }

// setLarge records whether the port behind conn takes whole TSO frames. A
// stale connection cannot clear the entry of the one that replaced it.
func (e *gsoEndpoint) setLarge(mac tcpip.LinkAddress, conn *ethernetConn, large bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if large {
		e.large[mac] = conn
	} else if e.large[mac] == conn {
		delete(e.large, mac)
	}
}

func (e *gsoEndpoint) takesLarge(mac tcpip.LinkAddress) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.large[mac] != nil
}

func (e *gsoEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	var out stack.PacketBufferList
	var owned []*stack.PacketBuffer
	defer func() {
		for _, p := range owned {
			p.DecRef()
		}
	}()
	for _, p := range pkts.AsSlice() {
		switch {
		case p.GSOOptions.Type == stack.GSONone || !p.GSOOptions.NeedsCsum:
			out.PushBack(p)
		case uint32(p.Size()) <= e.MTU():
			completeTCPChecksum(p)
			out.PushBack(p)
		case e.takesLarge(p.EgressRoute.RemoteLinkAddress):
			out.PushBack(p)
		default:
			for _, seg := range segmentTCPv4(p.ToView().AsSlice(), int(p.GSOOptions.MSS)) {
				s := stack.NewPacketBuffer(stack.PacketBufferOptions{ReserveHeaderBytes: header.EthernetMinimumSize, Payload: buffer.MakeWithData(seg)})
				s.NetworkProtocolNumber = p.NetworkProtocolNumber
				s.EgressRoute = p.EgressRoute
				owned = append(owned, s)
				out.PushBack(s)
			}
		}
	}
	if _, err := e.LinkEndpoint.WritePackets(out); err != nil {
		return 0, err
	}
	return pkts.Len(), nil
}

// completeTCPChecksum finishes a CHECKSUM_PARTIAL segment, whose checksum
// field holds the pseudo-header sum.
func completeTCPChecksum(p *stack.PacketBuffer) {
	tcp := header.TCP(p.TransportHeader().Slice())
	if len(tcp) < header.TCPMinimumSize {
		return
	}
	xsum := tcp.Checksum()
	tcp.SetChecksum(0)
	xsum = checksum.Combine(xsum, p.Data().Checksum())
	tcp.SetChecksum(^tcp.CalculateChecksum(xsum))
	p.GSOOptions = stack.GSO{}
}

// segmentTCPv4 splits an IPv4/TCP packet with a partial checksum into
// packets carrying at most mss payload bytes each, with complete checksums.
// FIN and PSH stay on the last segment and CWR on the first, as TSO does.
// It returns nil for anything that is not such a packet.
func segmentTCPv4(pkt []byte, mss int) [][]byte {
	if len(pkt) < header.IPv4MinimumSize || mss <= 0 {
		return nil
	}
	ip := header.IPv4(pkt)
	ihl := int(ip.HeaderLength())
	if ip.TransportProtocol() != header.TCPProtocolNumber || ihl < header.IPv4MinimumSize || len(pkt) < ihl+header.TCPMinimumSize {
		return nil
	}
	tcp := header.TCP(pkt[ihl:])
	doff := int(tcp.DataOffset())
	if doff < header.TCPMinimumSize || len(pkt) < ihl+doff {
		return nil
	}
	headers := ihl + doff
	payload := pkt[headers:]
	var segs [][]byte
	for off := 0; off < len(payload); off += mss {
		n := min(mss, len(payload)-off)
		seg := make([]byte, headers+n)
		copy(seg, pkt[:headers])
		copy(seg[headers:], payload[off:off+n])
		sip := header.IPv4(seg)
		sip.SetTotalLength(uint16(len(seg)))
		sip.SetID(ip.ID() + uint16(len(segs)))
		sip.SetChecksum(0)
		sip.SetChecksum(^sip.CalculateChecksum())
		st := header.TCP(seg[ihl:])
		binary.BigEndian.PutUint32(st[4:], tcp.SequenceNumber()+uint32(off))
		flags := tcp.Flags()
		if off+n < len(payload) {
			flags &^= header.TCPFlagFin | header.TCPFlagPsh
		}
		if off > 0 {
			flags &^= header.TCPFlagCwr
		}
		st.SetFlags(uint8(flags))
		st.SetChecksum(0)
		xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, sip.SourceAddress(), sip.DestinationAddress(), uint16(len(seg)-ihl))
		st.SetChecksum(^checksum.Checksum(seg[ihl:], xsum))
		segs = append(segs, seg)
	}
	return segs
}
