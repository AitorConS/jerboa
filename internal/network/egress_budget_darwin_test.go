package network

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// egressStack is a gateway stack with the owned egress forwarder installed,
// driven at the IP level: tests play the guests by injecting segments.
type egressStack struct {
	stack *stack.Stack
	link  *channel.Endpoint
	pool  *nativeEgressPool
	port  uint16 // host listener the gateway address forwards to
}

var egressGateway = [4]byte{172, 29, 0, 1}

func newEgressStack(t *testing.T, sources ...string) *egressStack {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	s := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol}})
	link := channel.New(1024, 1500, "")
	require.Nil(t, s.CreateNIC(1, link))
	require.Nil(t, s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddrFrom4(egressGateway).WithPrefix()}, stack.AddressProperties{}))
	sn, e := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{172, 29, 0, 0}), tcpip.MaskFromBytes([]byte{255, 255, 255, 0}))
	require.Nil(t, e)
	s.SetRouteTable([]tcpip.Route{{Destination: sn, NIC: 1}})
	p := newEgressPool()
	for _, source := range sources {
		p.activateSource(source)
	}
	p.install(s, net.IP(egressGateway[:]).String(), func(stack.TransportEndpointID, bool) bool { return true })
	t.Cleanup(func() { p.closeSource(""); s.Close(); link.Close() })
	return &egressStack{stack: s, link: link, pool: p, port: uint16(ln.Addr().(*net.TCPAddr).Port)}
}

// send injects a TCP segment from guest src:srcPort to the gateway's port.
func (e *egressStack) send(src [4]byte, srcPort uint16, seq, ack uint32, flags header.TCPFlags) {
	const ihl, doff = header.IPv4MinimumSize, header.TCPMinimumSize
	p := make([]byte, ihl+doff)
	ip := header.IPv4(p)
	ip.Encode(&header.IPv4Fields{TotalLength: uint16(len(p)), TTL: 64, Protocol: uint8(header.TCPProtocolNumber), SrcAddr: tcpip.AddrFrom4(src), DstAddr: tcpip.AddrFrom4(egressGateway)})
	ip.SetChecksum(^ip.CalculateChecksum())
	h := header.TCP(p[ihl:])
	h.Encode(&header.TCPFields{SrcPort: srcPort, DstPort: e.port, SeqNum: seq, AckNum: ack, DataOffset: doff, Flags: flags, WindowSize: 65535})
	h.SetChecksum(^h.CalculateChecksum(header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), doff)))
	e.link.InjectInbound(ipv4.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(p)}))
}

// synAcks collects the SYN-ACKs the gateway sends within d, by destination
// address and port, with the gateway's sequence number.
func (e *egressStack) synAcks(d time.Duration) map[tcpip.FullAddress]uint32 {
	return e.synAcksUntil(d, 0)
}

// synAcksUntil collects SYN-ACKs for d, or until it has seen want of them
// when want is not zero: a test that expects a number of answers waits for
// them as long as a loaded machine needs, not for a fixed time.
func (e *egressStack) synAcksUntil(d time.Duration, want int) map[tcpip.FullAddress]uint32 {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	out := map[tcpip.FullAddress]uint32{}
	for {
		if want > 0 && len(out) == want {
			return out
		}
		pkt := e.link.ReadContext(ctx)
		if pkt == nil {
			return out
		}
		b := pkt.ToView().AsSlice()
		pkt.DecRef()
		ip := header.IPv4(b)
		h := header.TCP(b[ip.HeaderLength():])
		if h.Flags() == header.TCPFlagSyn|header.TCPFlagAck {
			out[tcpip.FullAddress{Addr: ip.DestinationAddress(), Port: h.DestinationPort()}] = h.SequenceNumber()
		}
	}
}

func TestEgressPendingAttemptsAreBudgetedPerVM(t *testing.T) {
	a, b := [4]byte{172, 29, 0, 2}, [4]byte{172, 29, 0, 3}
	e := newEgressStack(t, "172.29.0.2", "172.29.0.3")

	// VM A opens 64 connections and never answers the SYN-ACKs.
	for i := range uint16(maxEgressFlowsPerVM) {
		e.send(a, 1000+i, 100, 0, header.TCPFlagSyn)
	}
	got := e.synAcks(500 * time.Millisecond)
	require.Len(t, got, maxEgressPendingPerVM, "A's unanswered attempts beyond its share are dropped")

	// VM B still connects.
	e.send(b, 2000, 100, 0, header.TCPFlagSyn)
	got = e.synAcks(2 * time.Second)
	_, ok := got[tcpip.FullAddress{Addr: tcpip.AddrFrom4(b), Port: 2000}]
	require.True(t, ok, "B's SYN must be answered while A holds its pending share")
}

func TestEgressEstablishedConnectionsDoNotHoldThePendingShare(t *testing.T) {
	a := [4]byte{172, 29, 0, 2}
	e := newEgressStack(t, "172.29.0.2")
	// Establish more connections than the pending share, a few at a time.
	const conns = maxEgressPendingPerVM + 8
	for i := uint16(0); i < conns; i += 4 {
		for j := i; j < i+4; j++ {
			e.send(a, 1000+j, 100, 0, header.TCPFlagSyn)
		}
		got := e.synAcks(500 * time.Millisecond)
		require.Len(t, got, 4)
		for j := i; j < i+4; j++ {
			seq := got[tcpip.FullAddress{Addr: tcpip.AddrFrom4(a), Port: 1000 + j}]
			e.send(a, 1000+j, 101, seq+1, header.TCPFlagAck)
		}
		require.Eventually(t, func() bool {
			e.pool.pending.mu.Lock()
			defer e.pool.pending.mu.Unlock()
			return e.pool.pending.held["172.29.0.2"] == 0
		}, 2*time.Second, time.Millisecond)
	}
	e.pool.mu.Lock()
	require.Equal(t, conns, e.pool.counts["172.29.0.2"])
	e.pool.mu.Unlock()
}

func TestEgressUnansweredHandshakeIsAbortedAfterItsDeadline(t *testing.T) {
	defer func(w time.Duration) { egressHandshakeWait = w }(egressHandshakeWait)
	egressHandshakeWait = 200 * time.Millisecond
	a := [4]byte{172, 29, 0, 2}
	e := newEgressStack(t, "172.29.0.2")
	for i := range uint16(maxEgressPendingPerVM) {
		e.send(a, 1000+i, 100, 0, header.TCPFlagSyn)
	}
	require.Len(t, e.synAcksUntil(5*time.Second, maxEgressPendingPerVM), maxEgressPendingPerVM)
	require.Eventually(t, func() bool {
		e.pool.mu.Lock()
		defer e.pool.mu.Unlock()
		e.pool.pending.mu.Lock()
		defer e.pool.pending.mu.Unlock()
		return len(e.pool.pending.held) == 0 && len(e.pool.flows) == 0
	}, 3*time.Second, 5*time.Millisecond, "aborted attempts release their share and host connection")

	// The VM can connect again.
	e.send(a, 3000, 100, 0, header.TCPFlagSyn)
	_, ok := e.synAcks(time.Second)[tcpip.FullAddress{Addr: tcpip.AddrFrom4(a), Port: 3000}]
	require.True(t, ok)
}

func TestNativeNetworkWithoutPolicyKeepsFlowCaps(t *testing.T) {
	// Host access (no address policy) widens where guests may connect, not
	// how many flows they may hold.
	n, err := NewNativeNetwork("172.29.0.0/24", "172.29.0.1", nil)
	require.NoError(t, err)
	defer n.Close()
	l, err := n.ListenVM(filepath.Join(shortDir(t), "vm.sock"), "172.29.0.2", "02:00:00:00:00:02")
	require.NoError(t, err)
	defer l.Close()
	for range maxEgressFlowsPerVM {
		require.NotNil(t, n.egress.reserve("172.29.0.2"))
	}
	require.Nil(t, n.egress.reserve("172.29.0.2"))

	require.NoError(t, n.Expose("tcp", "127.0.0.1:0", "172.29.0.2:8080"))
	require.NotNil(t, n.published["tcp/127.0.0.1:0"], "published ports use the capped owned forwarder")
	require.NoError(t, n.Unexpose("tcp", "127.0.0.1:0"))

	l.Close()
	n.egress.mu.Lock()
	defer n.egress.mu.Unlock()
	require.Empty(t, n.egress.flows, "removing the VM closes its flows")
}

func TestEgressPermittedWithoutPolicy(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("172.29.0.0/24")
	permitted := egressPermitted(subnet, "172.29.0.1", nil)
	to := func(dst [4]byte, port uint16) stack.TransportEndpointID {
		return stack.TransportEndpointID{LocalAddress: tcpip.AddrFrom4(dst), LocalPort: port, RemoteAddress: tcpip.AddrFrom4([4]byte{172, 29, 0, 2}), RemotePort: 40000}
	}
	require.False(t, permitted(to([4]byte{169, 254, 169, 254}, 80), false), "metadata services stay out of reach")
	require.False(t, permitted(to([4]byte{255, 255, 255, 255}, 67), true))
	require.True(t, permitted(to([4]byte{172, 29, 0, 1}, 80), false), "host access reaches the host's loopback")
	require.True(t, permitted(to([4]byte{192, 168, 1, 10}, 443), false))
	require.True(t, permitted(to([4]byte{1, 1, 1, 1}, 53), true))

	policy := egressPermitted(subnet, "172.29.0.1", func(_, address string, _ uint16, _ bool) bool { return address == "127.0.0.1" })
	require.True(t, policy(to([4]byte{172, 29, 0, 1}, 80), false), "the gateway is the host's loopback")
	require.False(t, policy(to([4]byte{172, 29, 0, 9}, 80), false), "the network's own addresses are never dialed on the host")
}
