package network

// The macOS backend uses gVisor's userspace IP stack and gvproxy's Ethernet
// switch. Each logical network has its own stack; no host routes or TAP driver
// are required. All sockets and goroutines are owned by the network lifecycle.
import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/AitorConS/jerboa/internal/netconst"
	"github.com/containers/gvisor-tap-vsock/pkg/services/forwarder"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

type NativeNetwork struct {
	stack *stack.Stack
	sw    *tap.Switch
	ports *forwarder.PortsForwarder
	dns   []net.PacketConn
	once  sync.Once
}

func NewNativeNetwork(cidr, gateway string, answer func([]byte, string) ([]byte, error)) (*NativeNetwork, error) {
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil || subnet.IP.To4() == nil || !subnet.Contains(net.ParseIP(gateway)) {
		return nil, fmt.Errorf("invalid native subnet/gateway %s/%s", cidr, gateway)
	}
	ep, err := tap.NewLinkEndpoint(false, 1500, "02:4a:65:72:62:01", gateway, []string{netconst.DNSAnycastIP, "10.0.2.3"})
	if err != nil {
		return nil, err
	}
	sw := tap.NewSwitch(false)
	ep.Connect(sw)
	sw.Connect(ep)
	s := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4}})
	fail := func(err error) (*NativeNetwork, error) { s.Close(); return nil, err }
	if e := s.CreateNIC(1, ep); e != nil {
		return fail(fmt.Errorf("native NIC: %s", e))
	}
	for _, ip := range []string{gateway, netconst.DNSAnycastIP, "10.0.2.3"} {
		if e := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddrFrom4Slice(net.ParseIP(ip).To4()).WithPrefix()}, stack.AddressProperties{}); e != nil {
			return fail(fmt.Errorf("native address: %s", e))
		}
	}
	_ = s.SetSpoofing(1, true)
	_ = s.SetPromiscuousMode(1, true)
	sn, e := tcpip.NewSubnet(tcpip.AddrFrom4Slice(subnet.IP.To4()), tcpip.MaskFromBytes(subnet.Mask))
	if e != nil {
		return fail(e)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: sn, NIC: 1}})
	var natMu sync.Mutex
	nat := map[tcpip.Address]tcpip.Address{tcpip.AddrFrom4Slice(net.ParseIP(gateway).To4()): tcpip.AddrFrom4Slice(net.IPv4(127, 0, 0, 1).To4())}
	tf := forwarder.TCP(s, nat, &natMu, false)
	uf := forwarder.UDP(s, nat, &natMu, false)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tf.HandlePacket)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, uf.HandlePacket)
	n := &NativeNetwork{stack: s, sw: sw, ports: forwarder.NewPortsForwarder(s)}
	// Bind each address explicitly so replies preserve the queried source IP.
	for _, address := range []string{netconst.DNSAnycastIP, "10.0.2.3"} {
		dns, err := gonet.DialUDP(s, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4Slice(net.ParseIP(address).To4()), Port: 53}, nil, ipv4.ProtocolNumber)
		if err != nil {
			return fail(err)
		}
		n.dns = append(n.dns, dns)
		go func() {
			slots := make(chan struct{}, 64)
			for {
				b := make([]byte, 4096)
				size, src, err := dns.ReadFrom(b)
				if err != nil {
					return
				}
				select {
				case slots <- struct{}{}:
					go func() {
						defer func() { <-slots }()
						ip, _, _ := net.SplitHostPort(src.String())
						if answer != nil {
							if resp, err := answer(b[:size], ip); err == nil {
								_, _ = dns.WriteTo(resp, src)
							}
						}
					}()
				default:
				}
			}
		}()
	}
	return n, nil
}
func (n *NativeNetwork) DialContext(ctx context.Context, _, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	p, err := net.LookupPort("tcp", port)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return nil, fmt.Errorf("invalid guest IP %s", host)
	}
	return gonet.DialContextTCP(ctx, n.stack, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4Slice(ip), Port: uint16(p)}, ipv4.ProtocolNumber)
}
func (n *NativeNetwork) Expose(proto, local, remote string) error {
	return n.ports.Expose(types.TransportProtocol(proto), local, remote)
}
func (n *NativeNetwork) Unexpose(proto, local string) error {
	return n.ports.Unexpose(types.TransportProtocol(proto), local)
}
func (n *NativeNetwork) Close() {
	n.once.Do(func() {
		for _, c := range n.dns {
			_ = c.Close()
		}
		n.stack.Close()
	})
}

type NativeLink struct {
	listener net.Listener
	mu       sync.Mutex
	conn     net.Conn
	closed   bool
	rx, tx   atomic.Int64
}
type frameCounter struct {
	header    [4]byte
	used      int
	remaining uint32
}

func (f *frameCounter) count(p []byte) int64 {
	var payload int64
	for len(p) > 0 {
		if f.remaining > 0 {
			n := min(len(p), int(f.remaining))
			payload += int64(n)
			p = p[n:]
			f.remaining -= uint32(n)
			continue
		}
		n := copy(f.header[f.used:], p)
		f.used += n
		p = p[n:]
		if f.used == 4 {
			f.remaining = binary.BigEndian.Uint32(f.header[:])
			f.used = 0
		}
	}
	return payload
}

type countedConn struct {
	net.Conn
	link                    *NativeLink
	readFrames, writeFrames frameCounter
}

func (c *countedConn) Read(b []byte) (int, error) {
	n, e := c.Conn.Read(b)
	c.link.tx.Add(c.readFrames.count(b[:n]))
	return n, e
}
func (c *countedConn) Write(b []byte) (int, error) {
	n, e := c.Conn.Write(b)
	c.link.rx.Add(c.writeFrames.count(b[:n]))
	return n, e
}
func (n *NativeNetwork) Listen(path string) (*NativeLink, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	l := &NativeLink{listener: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.mu.Lock()
			if l.closed {
				l.mu.Unlock()
				c.Close()
				return
			}
			l.conn = c
			l.mu.Unlock()
			_ = n.sw.Accept(context.Background(), &countedConn{Conn: c, link: l}, types.QemuProtocol)
		}
	}()
	return l, nil
}
func (l *NativeLink) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	_ = l.listener.Close()
	if l.conn != nil {
		_ = l.conn.Close()
	}
}
func (l *NativeLink) Stats() (int64, int64) { return l.rx.Load(), l.tx.Load() }
