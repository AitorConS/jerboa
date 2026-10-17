package network

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// tcpPacket builds an IPv4/TCP packet with a partial (pseudo-header)
// checksum, as the gateway's TCP emits it with GSO.
func tcpPacket(payload int, flags header.TCPFlags) []byte {
	const ihl, doff = 20, 32
	p := make([]byte, ihl+doff+payload)
	ip := header.IPv4(p)
	ip.Encode(&header.IPv4Fields{TotalLength: uint16(len(p)), ID: 7, TTL: 64, Protocol: uint8(header.TCPProtocolNumber),
		SrcAddr: tcpip.AddrFrom4([4]byte{10, 100, 0, 1}), DstAddr: tcpip.AddrFrom4([4]byte{10, 100, 0, 2})})
	ip.SetChecksum(^ip.CalculateChecksum())
	tcp := header.TCP(p[ihl:])
	tcp.Encode(&header.TCPFields{SrcPort: 5201, DstPort: 40000, SeqNum: 1000, AckNum: 9, DataOffset: doff, Flags: flags, WindowSize: 512})
	for i := range payload {
		p[ihl+doff+i] = byte(i * 7)
	}
	tcp.SetChecksum(header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(p)-ihl)))
	return p
}

func TestSegmentTCPv4(t *testing.T) {
	const mss = 8948
	pkt := tcpPacket(3*mss+100, header.TCPFlagAck|header.TCPFlagPsh|header.TCPFlagFin|header.TCPFlagCwr)
	segs := segmentTCPv4(pkt, mss)
	require.Len(t, segs, 4)
	var payload []byte
	for i, s := range segs {
		ip := header.IPv4(s)
		require.Equal(t, len(s), int(ip.TotalLength()))
		require.True(t, ip.IsChecksumValid(), "segment %d IPv4 checksum", i)
		require.Equal(t, uint16(7+i), ip.ID())
		tcp := header.TCP(s[20:])
		require.Equal(t, uint32(1000+i*mss), tcp.SequenceNumber())
		xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(s)-20))
		require.Equal(t, uint16(0xffff), checksum.Checksum(s[20:], xsum), "segment %d TCP checksum", i)
		last := i == len(segs)-1
		require.Equal(t, last, tcp.Flags().Contains(header.TCPFlagFin), "FIN only on the last segment")
		require.Equal(t, last, tcp.Flags().Contains(header.TCPFlagPsh), "PSH only on the last segment")
		require.Equal(t, i == 0, tcp.Flags().Contains(header.TCPFlagCwr), "CWR only on the first segment")
		require.True(t, tcp.Flags().Contains(header.TCPFlagAck))
		payload = append(payload, s[52:]...)
	}
	require.Equal(t, pkt[52:], payload)

	require.Nil(t, segmentTCPv4(pkt[:30], mss), "truncated")
	udp := append([]byte(nil), pkt...)
	udp[9] = uint8(header.UDPProtocolNumber)
	require.Nil(t, segmentTCPv4(udp, mss), "not TCP")
}

func TestEthernetSwitchCapabilities(t *testing.T) {
	a, b := net.Pipe()
	c := newEthernetConn(a, "172.20.0.2", "02:01:02:03:04:05")
	t.Cleanup(func() { c.Close(); b.Close() })
	got := make(chan byte, 4)
	c.capabilities = func(flags byte) { got <- flags }

	// The link opens with jerboad's capabilities, addressed to the VM.
	open := make([]byte, 4+switchCapsSize)
	_, err := io.ReadFull(b, open)
	require.NoError(t, err)
	f := open[4:]
	require.Equal(t, []byte(c.mac), f[0:6])
	require.Equal(t, []byte(gatewayMAC), f[6:12])
	require.Equal(t, uint16(switchCapsType), binary.BigEndian.Uint16(f[12:14]))
	require.Equal(t, switchCapsMagic, string(f[14:18]))
	require.Equal(t, []byte{1, switchCapsLarge}, f[18:20])
	require.Equal(t, uint16(9000), binary.BigEndian.Uint16(f[20:22]))

	answer := func(src net.HardwareAddr, version byte) []byte {
		a := make([]byte, 4+switchCapsSize)
		binary.BigEndian.PutUint32(a, switchCapsSize)
		copy(a[4:10], gatewayMAC)
		copy(a[10:16], src)
		binary.BigEndian.PutUint16(a[16:18], switchCapsType)
		copy(a[18:22], switchCapsMagic)
		a[22], a[23] = version, switchCapsLarge
		return a
	}
	valid := make([]byte, 4+34)
	binary.BigEndian.PutUint32(valid, 34)
	copy(valid[10:16], c.mac)
	binary.BigEndian.PutUint16(valid[16:18], 0x0800)
	valid[18] = 0x45
	copy(valid[30:34], c.ip)
	foreign := net.HardwareAddr{2, 9, 9, 9, 9, 9}
	go func() {
		_, _ = b.Write(bytes.Join([][]byte{answer(foreign, 1), answer(c.mac, 2), answer(c.mac, 1), valid}, nil))
	}()
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	require.NoError(t, err)
	require.Equal(t, c.announcement(), buf[:n])
	n, err = c.Read(buf)
	require.NoError(t, err)
	// Capability frames never reach the switch; only the port's own,
	// well-formed answer is reported.
	require.Equal(t, valid, buf[:n])
	require.Equal(t, byte(switchCapsLarge), <-got)
	require.Empty(t, got)
}

func TestValidLarge(t *testing.T) {
	f := make([]byte, 14+20000)
	copy(f[0:6], gatewayMAC)
	binary.BigEndian.PutUint16(f[12:14], 0x0800)
	f[14] = 0x45
	binary.BigEndian.PutUint16(f[16:18], 20000)
	f[23] = 6
	require.True(t, validLarge(f))
	require.True(t, validLarge(f[:14+9000]), "MTU-sized frames are not restricted")
	f[16]++
	require.False(t, validLarge(f), "inconsistent IPv4 length")
	f[16]--
	f[23] = 17
	require.False(t, validLarge(f), "only TCP")
	f[23] = 6
	f[0] = 0x04
	require.False(t, validLarge(f), "only to the gateway")
}

func TestGSOEndpointStaleLinkKeepsReplacement(t *testing.T) {
	e := newGSOEndpoint(nil)
	port := tcpip.LinkAddress([]byte{2, 1, 2, 3, 4, 5})
	old, cur := &ethernetConn{}, &ethernetConn{}
	e.setLarge(port, old, true)
	e.setLarge(port, cur, true)
	e.setLarge(port, old, false)
	require.True(t, e.takesLarge(port), "the old link closing must not clear its replacement")
	e.setLarge(port, cur, false)
	require.False(t, e.takesLarge(port))
}
