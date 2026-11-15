package network

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Owned host flows for policy-enforced networks. Reservations include pending
// dials. Removing a VM closes its flows even when other VMs keep the stack alive.
type nativeEgressPool struct {
	mu     sync.Mutex
	flows  map[*nativeFlow]bool
	counts map[string]int
	// Published host connections have a separate budget from guest egress.
	// A busy published port must not consume the guest's egress allowance.
	publishedCounts map[string]int
	egressTotal     int
	publishedTotal  int
	sources         map[string]bool
	// pending bounds each VM's TCP connection attempts that are still being
	// set up; the forwarder admits a network-wide number of them.
	pending *sourceBudget
	closed  bool
}

const (
	maxEgressFlowsPerVM    = 64
	maxEgressFlows         = 256
	maxPublishedFlowsPerVM = 512
	maxPublishedFlows      = 1024
	// A connection attempt holds a forwarder slot from the guest's SYN until
	// the host dial and the guest's handshake are done. One VM may hold a
	// sixteenth of the network's attempts, so a guest that leaves its
	// handshakes unanswered cannot keep the other VMs from connecting.
	maxEgressPendingPerVM = maxEgressFlows / 16
)

// egressHandshakeWait bounds how long an attempt waits for the guest's final
// ACK after the host dial; gVisor alone retransmits the SYN-ACK for about two
// minutes.
var egressHandshakeWait = 5 * time.Second

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
	return &nativeEgressPool{flows: map[*nativeFlow]bool{}, counts: map[string]int{}, publishedCounts: map[string]int{}, sources: map[string]bool{}, pending: newSourceBudget(maxEgressPendingPerVM)}
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
	if p.closed || !p.sources[source] {
		return nil
	}
	if tag == "" && (p.egressTotal >= maxEgressFlows || p.counts[source] >= maxEgressFlowsPerVM) {
		return nil
	}
	if tag != "" && (p.publishedTotal >= maxPublishedFlows || p.publishedCounts[source] >= maxPublishedFlowsPerVM) {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &nativeFlow{pool: p, source: source, tag: tag, ctx: ctx, cancel: cancel}
	p.flows[f] = true
	if tag == "" {
		p.counts[source]++
		p.egressTotal++
	} else {
		p.publishedCounts[source]++
		p.publishedTotal++
	}
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
		if f.tag == "" {
			f.pool.counts[f.source]--
			f.pool.egressTotal--
			if f.pool.counts[f.source] == 0 {
				delete(f.pool.counts, f.source)
			}
		} else {
			f.pool.publishedCounts[f.source]--
			f.pool.publishedTotal--
			if f.pool.publishedCounts[f.source] == 0 {
				delete(f.pool.publishedCounts, f.source)
			}
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
		_, _ = copyStream(dst, src)
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
	tf := tcp.NewForwarder(s, 0, maxEgressFlows, func(r *tcp.ForwarderRequest) {
		source := r.ID().RemoteAddress.String()
		if !p.pending.acquire(source) {
			// Like a full forwarder: drop the SYN, the guest retransmits it.
			r.Complete(false)
			return
		}
		// The VM's pending share is released with the forwarder slot, not
		// when the connection ends: duplex below runs for its whole life.
		complete := func(reset bool) {
			r.Complete(reset)
			p.pending.release(source)
		}
		f := p.reserve(source)
		if f == nil {
			complete(true)
			return
		}
		host, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(f.ctx, "tcp4", destination(r.ID()))
		if err != nil {
			complete(true)
			f.close()
			return
		}
		if !f.add(host) {
			complete(true)
			return
		}
		var wq waiter.Queue
		ep, e := createEndpointWithin(s, r, &wq, egressHandshakeWait)
		if e != nil {
			complete(true)
			f.close()
			return
		}
		complete(false)
		guest := gonet.NewTCPConn(&wq, ep)
		if !f.add(guest) {
			return
		}
		f.duplex(host, guest)
	})
	uf := udp.NewForwarder(s, func(r *udp.ForwarderRequest) bool {
		f := p.reserve(r.ID().RemoteAddress.String())
		if f == nil {
			return true
		}
		var wq waiter.Queue
		ep, e := r.CreateEndpoint(&wq)
		if e != nil {
			f.close()
			return true
		}
		guest := gonet.NewUDPConn(&wq, ep)
		if !f.add(guest) {
			return true
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
		return true
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

// createEndpointWithin completes the guest's handshake for r, aborting it if
// the guest has not sent its final ACK within wait. gVisor blocks in
// CreateEndpoint until the handshake ends and has no deadline of its own
// shorter than its SYN-ACK retransmissions.
func createEndpointWithin(s *stack.Stack, r *tcp.ForwarderRequest, wq *waiter.Queue, wait time.Duration) (tcpip.Endpoint, tcpip.Error) {
	id := r.ID()
	timer := time.AfterFunc(wait, func() {
		// While the handshake runs, its endpoint is registered under the
		// request's ID. Only an endpoint still in SYN-RCVD is aborted, so a
		// handshake that completed at the deadline keeps its connection.
		ep, ok := s.FindTransportEndpoint(ipv4.ProtocolNumber, tcp.ProtocolNumber, id, 1).(*tcp.Endpoint)
		if ok && tcp.EndpointState(ep.State()) == tcp.StateSynRecv {
			ep.Abort()
		}
	})
	defer timer.Stop()
	return r.CreateEndpoint(wq)
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
		_, err := copyStream(dst, src)
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

// streamCopySize is the chunk the forwarder moves between a guest connection
// and a host socket. gVisor hands over whole TSO segments (up to 64 KB) since
// the gateway takes them, and io.Copy's 32 KiB buffer split each of them into
// two reads and two host writes.
const streamCopySize = 256 << 10

var streamCopyBuffers = sync.Pool{New: func() any {
	b := make([]byte, streamCopySize)
	return &b
}}

// copyStream copies src to dst through a pooled streamCopySize buffer. The
// plain Reader and Writer views keep io.CopyBuffer from taking the
// ReaderFrom/WriterTo shortcuts, which bring back a 32 KiB buffer here.
func copyStream(dst, src net.Conn) (int64, error) {
	b := streamCopyBuffers.Get().(*[]byte)
	defer streamCopyBuffers.Put(b)
	n, err := io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, *b)
	if err != nil {
		return n, fmt.Errorf("copy stream: %w", err)
	}
	return n, nil
}
