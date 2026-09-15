package network

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Owned host flows for policy-enforced networks. Reservations include pending
// dials. Removing a VM closes its flows even when other VMs keep the stack alive.
type nativeEgressPool struct {
	mu      sync.Mutex
	flows   map[*nativeFlow]bool
	counts  map[string]int
	sources map[string]bool
	closed  bool
}
type nativeFlow struct {
	pool   *nativeEgressPool
	source string
	tag    string
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
	once   sync.Once
}

func newEgressPool() *nativeEgressPool {
	return &nativeEgressPool{flows: map[*nativeFlow]bool{}, counts: map[string]int{}, sources: map[string]bool{}}
}
func (p *nativeEgressPool) activateSource(source string) {
	p.mu.Lock()
	p.sources[source] = true
	p.mu.Unlock()
}
func (p *nativeEgressPool) reserve(source string) *nativeFlow { return p.reserveTagged(source, "") }
func (p *nativeEgressPool) reserveTagged(source, tag string) *nativeFlow {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.sources[source] || len(p.flows) >= 256 || p.counts[source] >= 64 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &nativeFlow{pool: p, source: source, tag: tag, ctx: ctx, cancel: cancel}
	p.flows[f] = true
	p.counts[source]++
	return f
}
func (f *nativeFlow) add(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		c.Close()
		return false
	}
	f.conns = append(f.conns, c)
	return true
}
func (f *nativeFlow) close() {
	f.once.Do(func() {
		f.cancel()
		f.mu.Lock()
		f.closed = true
		for _, c := range f.conns {
			c.Close()
		}
		f.mu.Unlock()
		f.pool.mu.Lock()
		delete(f.pool.flows, f)
		f.pool.counts[f.source]--
		if f.pool.counts[f.source] == 0 {
			delete(f.pool.counts, f.source)
		}
		f.pool.mu.Unlock()
	})
}
func (p *nativeEgressPool) closeSource(source string) {
	p.mu.Lock()
	var flows []*nativeFlow
	if source == "" {
		p.closed = true
	}
	delete(p.sources, source)
	for f := range p.flows {
		if source == "" || f.source == source {
			flows = append(flows, f)
		}
	}
	p.mu.Unlock()
	for _, f := range flows {
		f.close()
	}
}
func (f *nativeFlow) pump(dst, src net.Conn, datagram bool) {
	defer f.close()
	if !datagram {
		_, _ = io.Copy(dst, src)
		return
	}
	b := make([]byte, 65536)
	for {
		src.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := src.Read(b)
		if err != nil {
			return
		}
		written, err := dst.Write(b[:n])
		if err != nil || written != n {
			return
		}
	}
}
func (p *nativeEgressPool) install(s *stack.Stack, gateway string, permitted func(stack.TransportEndpointID, bool) bool) {
	destination := func(id stack.TransportEndpointID) string {
		ip := id.LocalAddress.String()
		if ip == gateway {
			ip = "127.0.0.1"
		}
		return net.JoinHostPort(ip, strconv.Itoa(int(id.LocalPort)))
	}
	tf := tcp.NewForwarder(s, 0, 64, func(r *tcp.ForwarderRequest) {
		f := p.reserve(r.ID().RemoteAddress.String())
		if f == nil {
			r.Complete(true)
			return
		}
		host, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(f.ctx, "tcp4", destination(r.ID()))
		if err != nil {
			r.Complete(true)
			f.close()
			return
		}
		if !f.add(host) {
			r.Complete(true)
			return
		}
		var wq waiter.Queue
		ep, e := r.CreateEndpoint(&wq)
		if e != nil {
			r.Complete(true)
			f.close()
			return
		}
		r.Complete(false)
		guest := gonet.NewTCPConn(&wq, ep)
		if !f.add(guest) {
			return
		}
		f.duplex(host, guest)
	})
	uf := udp.NewForwarder(s, func(r *udp.ForwarderRequest) {
		f := p.reserve(r.ID().RemoteAddress.String())
		if f == nil {
			return
		}
		var wq waiter.Queue
		ep, e := r.CreateEndpoint(&wq)
		if e != nil {
			f.close()
			return
		}
		guest := gonet.NewUDPConn(&wq, ep)
		if !f.add(guest) {
			return
		}
		address := destination(r.ID())
		go func() {
			host, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(f.ctx, "udp4", address)
			if err != nil {
				f.close()
				return
			}
			if !f.add(host) {
				return
			}
			go f.pump(host, guest, true)
			f.pump(guest, host, true)
		}()
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		if !permitted(id, false) {
			return false
		}
		return tf.HandlePacket(id, pkt)
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		if !permitted(id, true) {
			return false
		}
		return uf.HandlePacket(id, pkt)
	})
}

func (p *nativeEgressPool) closeTag(tag string) {
	p.mu.Lock()
	var flows []*nativeFlow
	for f := range p.flows {
		if f.tag == tag {
			flows = append(flows, f)
		}
	}
	p.mu.Unlock()
	for _, f := range flows {
		f.close()
	}
}

// Propagate FIN independently in each direction; a client that closes its
// write half must still be able to receive the server's final response.
func (f *nativeFlow) duplex(a, b net.Conn) {
	defer f.close()
	done := make(chan struct{}, 2)
	copyHalf := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err == nil {
			if c, ok := dst.(interface{ CloseWrite() error }); ok {
				err = c.CloseWrite()
			} else {
				err = net.ErrClosed
			}
		}
		if err != nil {
			f.close()
		}
		done <- struct{}{}
	}
	go copyHalf(a, b)
	go copyHalf(b, a)
	<-done
	<-done
}
