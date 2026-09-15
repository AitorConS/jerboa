package network

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

type nativePublication struct {
	mu          sync.Mutex
	closed      bool
	listener    interface{ Close() error }
	pool        *nativeEgressPool
	source, tag string
}

func (p *nativePublication) reserve() *nativeFlow {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	return p.pool.reserveTagged(p.source, p.tag)
}
func (p *nativePublication) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.listener.Close()
	p.mu.Unlock()
	p.pool.closeTag(p.tag)
}
func (n *NativeNetwork) exposeOwned(proto, local, remote string) error {
	n.publishMu.Lock()
	defer n.publishMu.Unlock()
	key := proto + "/" + local
	if n.published == nil {
		n.published = map[string]*nativePublication{}
	}
	if n.published[key] != nil {
		return fmt.Errorf("port already exposed")
	}
	ip, port, err := net.SplitHostPort(remote)
	if err != nil {
		return err
	}
	p := &nativePublication{pool: n.egress, source: ip, tag: key}
	switch proto {
	case "tcp":
		listener, err := net.Listen("tcp4", local)
		if err != nil {
			return err
		}
		p.listener = listener
		go func() {
			for {
				host, err := listener.Accept()
				if err != nil {
					return
				}
				f := p.reserve()
				if f == nil {
					host.Close()
					continue
				}
				if !f.add(host) {
					continue
				}
				go func() {
					ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
					defer cancel()
					guest, err := n.DialContext(ctx, "tcp", remote)
					if err != nil {
						f.close()
						return
					}
					if !f.add(guest) {
						return
					}
					f.duplex(guest, host)
				}()
			}
		}()
	case "udp":
		addr, err := net.ResolveUDPAddr("udp4", local)
		if err != nil {
			return err
		}
		listener, err := net.ListenUDP("udp4", addr)
		if err != nil {
			return err
		}
		p.listener = listener
		guestPort, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			listener.Close()
			return err
		}
		var mu sync.Mutex
		clients := map[string]net.Conn{}
		go func() {
			b := make([]byte, 65536)
			for {
				size, src, err := listener.ReadFromUDP(b)
				if err != nil {
					return
				}
				key := src.String()
				mu.Lock()
				guest := clients[key]
				mu.Unlock()
				if guest == nil {
					f := p.reserve()
					if f == nil {
						continue
					}
					c, err := gonet.DialUDP(n.stack, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4Slice(net.ParseIP(ip).To4()), Port: uint16(guestPort)}, ipv4.ProtocolNumber)
					if err != nil {
						f.close()
						continue
					}
					if !f.add(c) {
						continue
					}
					guest = c
					mu.Lock()
					clients[key] = guest
					mu.Unlock()
					go func(c net.Conn, src *net.UDPAddr) {
						defer f.close()
						defer func() { mu.Lock(); delete(clients, src.String()); mu.Unlock() }()
						reply := make([]byte, 65536)
						for {
							c.SetReadDeadline(time.Now().Add(30 * time.Second))
							size, err := c.Read(reply)
							if err != nil {
								return
							}
							if _, err := listener.WriteToUDP(reply[:size], src); err != nil {
								return
							}
						}
					}(guest, src)
				}
				guest.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_, _ = guest.Write(b[:size])
			}
		}()
	default:
		return fmt.Errorf("unsupported published protocol %q", proto)
	}
	n.published[key] = p
	return nil
}
func (n *NativeNetwork) unexposeOwned(proto, local string) error {
	n.publishMu.Lock()
	p := n.published[proto+"/"+local]
	delete(n.published, proto+"/"+local)
	n.publishMu.Unlock()
	if p != nil {
		p.close()
	}
	return nil
}
