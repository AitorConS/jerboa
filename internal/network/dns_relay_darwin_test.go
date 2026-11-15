package network

import (
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/netconst"
	"github.com/stretchr/testify/require"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// dnsQueryFrame is a framed Ethernet/IPv4/UDP datagram from a guest to the
// network's DNS relay address.
func dnsQueryFrame(mac net.HardwareAddr, ip string, port uint16, payload []byte) []byte {
	size := header.EthernetMinimumSize + header.IPv4MinimumSize + header.UDPMinimumSize + len(payload)
	f := make([]byte, 4+size)
	binary.BigEndian.PutUint32(f[:4], uint32(size))
	eth := header.Ethernet(f[4:])
	eth.Encode(&header.EthernetFields{SrcAddr: tcpip.LinkAddress(mac), DstAddr: tcpip.LinkAddress(gatewayMAC), Type: header.IPv4ProtocolNumber})
	ipv4 := header.IPv4(f[4+header.EthernetMinimumSize:])
	ipv4.Encode(&header.IPv4Fields{TotalLength: uint16(size - header.EthernetMinimumSize), TTL: 64, Protocol: uint8(header.UDPProtocolNumber),
		SrcAddr: tcpip.AddrFrom4Slice(net.ParseIP(ip).To4()), DstAddr: tcpip.AddrFrom4Slice(net.ParseIP(netconst.DNSAnycastIP).To4())})
	ipv4.SetChecksum(^ipv4.CalculateChecksum())
	udp := header.UDP(ipv4[header.IPv4MinimumSize:])
	udp.Encode(&header.UDPFields{SrcPort: port, DstPort: 53, Length: uint16(header.UDPMinimumSize + len(payload))})
	copy(udp[header.UDPMinimumSize:], payload) // checksum 0: none, valid for IPv4
	return f
}

func TestDNSRelaySlotsAreSharedPerVM(t *testing.T) {
	var mu sync.Mutex
	inFlight := map[string]int{}
	stuck := make(chan struct{})
	defer close(stuck)
	// VM A's lookups go to an upstream that never answers; B's are local.
	answer := func(q []byte, ip string) ([]byte, error) {
		mu.Lock()
		inFlight[ip]++
		mu.Unlock()
		if ip == "172.29.0.2" {
			<-stuck
		}
		return q, nil
	}
	n, err := NewNativeNetwork("172.29.0.0/24", "172.29.0.1", answer)
	require.NoError(t, err)
	defer n.Close()
	dir := shortDir(t)
	guest := func(name, ip, mac string) (net.Conn, net.HardwareAddr) {
		path := filepath.Join(dir, name)
		l, err := n.ListenVM(path, ip, mac)
		require.NoError(t, err)
		t.Cleanup(l.Close)
		c, err := net.Dial("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { c.Close() })
		go func() { _, _ = io.Copy(io.Discard, c) }()
		hw, _ := net.ParseMAC(mac)
		return c, hw
	}
	a, aMAC := guest("a.sock", "172.29.0.2", "02:00:00:00:00:02")
	b, bMAC := guest("b.sock", "172.29.0.3", "02:00:00:00:00:03")
	held := func(ip string) func() int {
		return func() int { mu.Lock(); defer mu.Unlock(); return inFlight[ip] }
	}

	for i := range uint16(dnsRelaySlots) {
		_, err := a.Write(dnsQueryFrame(aMAC, "172.29.0.2", 10000+i, []byte("query from a")))
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return held("172.29.0.2")() == dnsRelaySlotsPerVM }, 2*time.Second, time.Millisecond)

	_, err = b.Write(dnsQueryFrame(bMAC, "172.29.0.3", 20000, []byte("query from b")))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return held("172.29.0.3")() == 1 }, 2*time.Second, time.Millisecond,
		"B must be served while A's lookups are stuck upstream")
	require.Equal(t, dnsRelaySlotsPerVM, held("172.29.0.2")(), "A never holds more than its share")
}
