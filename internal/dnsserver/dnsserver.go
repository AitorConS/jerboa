//go:build linux || (darwin && arm64)

// Package dnsserver implements a small UDP DNS server that lets guest VMs
// resolve each other by name. Guests send queries to a fixed address the daemon
// owns (see netconst.DNSAnycastIP), reached through their default gateway. Each
// query is scoped to the caller's own network — inferred from the packet source
// IP — so "db" resolves to the db VM on the same network. Names the daemon does
// not own are forwarded to an upstream resolver so ordinary internet lookups
// keep working.
package dnsserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/AitorConS/jerboa/internal/scheduler"
)

// upstreamTimeout bounds a single forwarded query to an external resolver.
const upstreamTimeout = 3 * time.Second

// maxConcurrentQueries caps the number of queries handled in parallel. It backs
// a semaphore so a UDP flood cannot spawn unbounded goroutines; excess packets
// are dropped (DNS clients retry) rather than exhausting host memory/CPU.
const maxConcurrentQueries = 64

// maxUpstreamResponse is the largest answer relayed from the upstream
// resolver, the most a UDP datagram carries.
const maxUpstreamResponse = 65535

// Server answers guest DNS queries from the daemon's in-memory VM state.
type Server struct {
	resolver *scheduler.Resolver
	// upstream is the "host:port" of a fallback resolver for names the daemon
	// does not own. Empty disables forwarding (unknown names return NXDOMAIN).
	upstream string
	// mu guards conn: Close runs on another goroutine than ListenAndServe.
	mu   sync.Mutex
	conn *net.UDPConn
	// sem bounds concurrent query handlers (see maxConcurrentQueries).
	sem chan struct{}
	// turn rotates the order of the addresses a shared name answers with.
	turn atomic.Uint32
}

// New creates a Server backed by resolver, forwarding unknown names to upstream
// (e.g. "1.1.1.1:53"). Pass an empty upstream to disable forwarding.
func New(resolver *scheduler.Resolver, upstream string) *Server {
	return &Server{
		resolver: resolver,
		upstream: upstream,
		sem:      make(chan struct{}, maxConcurrentQueries),
	}
}

// ListenAndServe binds addr ("ip:port") and serves until the connection is
// closed. It blocks, so callers typically run it in a goroutine.
func (s *Server) ListenAndServe(addr string) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("dnsserver: resolve %s: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("dnsserver: listen %s: %w", addr, err)
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	slog.Info("guest dns server listening", "addr", addr)

	buf := make([]byte, 512)
	for {
		n, src, readErr := conn.ReadFromUDP(buf)
		if readErr != nil {
			// A closed connection (Close was called) ends the loop cleanly.
			if errors.Is(readErr, net.ErrClosed) {
				return nil
			}
			slog.Debug("dnsserver: read", "err", readErr)
			continue
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		// Bound concurrency: if the worker pool is saturated, drop the query
		// rather than spawn an unbounded goroutine under a flood. DNS clients
		// retry, so a dropped packet degrades gracefully.
		select {
		case s.sem <- struct{}{}:
			go func() {
				defer func() { <-s.sem }()
				s.handle(conn, query, src)
			}()
		default:
			slog.Debug("dnsserver: worker pool saturated, dropping query", "src", src.IP)
		}
	}
}

// Close stops the server.
func (s *Server) Close() error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		if err := conn.Close(); err != nil {
			return fmt.Errorf("dnsserver: close: %w", err)
		}
	}
	return nil
}

func (s *Server) handle(conn *net.UDPConn, query []byte, src *net.UDPAddr) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("dnsserver: recovered panic", "src", src.IP, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	resp, err := s.buildResponse(query, src.IP.String())
	if err != nil {
		slog.Debug("dnsserver: handle", "src", src.IP, "err", err)
		return
	}
	if _, err := conn.WriteToUDP(resp, src); err != nil {
		slog.Debug("dnsserver: write", "src", src.IP, "err", err)
	}
}

func (s *Server) Answer(query []byte, srcIP string) ([]byte, error) {
	return s.buildResponse(query, srcIP)
}

// buildResponse parses a query, answers A questions the daemon owns from
// srcIP's network, and forwards everything else upstream.
func (s *Server) buildResponse(query []byte, srcIP string) ([]byte, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil, fmt.Errorf("parse header: %w", err)
	}
	q, err := p.Question()
	if err != nil {
		return nil, fmt.Errorf("parse question: %w", err)
	}

	// Scope the lookup to the caller's own network. An empty network (source
	// IP not held by any running VM) must NOT fall through to Resolve, which
	// would search across every network and leak names between them; treat
	// it as not-owned and forward/NXDOMAIN below instead.
	if network := s.resolver.NetworkForIP(srcIP); network != "" {
		name := strings.TrimSuffix(q.Name.String(), ".")
		// Ownership is decided once, from every record of the network that
		// answers to the name (in any letter case), whatever the question's
		// type: the replicas of a service share an alias, and asking for
		// exactly one would fail on them. A name of the network stays on the
		// host. Forwarded, a public domain of the same name would answer (an
		// AAAA for "db.app"), and the upstream resolver would learn the
		// network's service names.
		if recs, rErr := s.resolver.ResolveAll(name, network); rErr == nil && len(recs) > 0 {
			if q.Type == dnsmessage.TypeA {
				if ips := ipv4s(recs); len(ips) > 0 {
					return s.answerA(hdr.ID, q, rotate(ips, int(s.turn.Add(1))))
				}
			}
			// The network has only IPv4 addresses, and this name none it
			// can tell: there is no such record, and the question stays
			// here.
			return s.empty(hdr.ID, q, dnsmessage.RCodeSuccess)
		}
	}

	// Not a name this network owns: forward upstream so
	// ordinary internet name resolution still works for the guest.
	if s.upstream != "" {
		if resp, fErr := s.forward(query); fErr == nil {
			return resp, nil
		}
	}
	return s.empty(hdr.ID, q, dnsmessage.RCodeNameError)
}

// maxAnswers bounds the addresses in one answer so it fits the 512 bytes
// every resolver accepts over UDP. With the order rotating, a larger set
// is still reached over successive questions.
const maxAnswers = 16

// ipv4s returns the IPv4 addresses of recs, leaving out records that hold
// none.
func ipv4s(recs []scheduler.Record) [][4]byte {
	ips := make([][4]byte, 0, len(recs))
	for _, rec := range recs {
		if ip := net.ParseIP(rec.IP).To4(); ip != nil {
			ips = append(ips, [4]byte{ip[0], ip[1], ip[2], ip[3]})
		}
	}
	return ips
}

// rotate returns ips starting at position turn, so successive answers lead
// with a different address and clients that take the first one spread out.
func rotate(ips [][4]byte, turn int) [][4]byte {
	start := turn % len(ips)
	out := make([][4]byte, 0, len(ips))
	out = append(out, ips[start:]...)
	out = append(out, ips[:start]...)
	if len(out) > maxAnswers {
		out = out[:maxAnswers]
	}
	return out
}

func (s *Server) answerA(id uint16, q dnsmessage.Question, ips [][4]byte) ([]byte, error) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 id,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   true,
		RecursionAvailable: true,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, fmt.Errorf("start questions: %w", err)
	}
	if err := b.Question(q); err != nil {
		return nil, fmt.Errorf("add question: %w", err)
	}
	if err := b.StartAnswers(); err != nil {
		return nil, fmt.Errorf("start answers: %w", err)
	}
	for _, ip := range ips {
		err := b.AResource(dnsmessage.ResourceHeader{
			Name:  q.Name,
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
			TTL:   30,
		}, dnsmessage.AResource{A: ip})
		if err != nil {
			return nil, fmt.Errorf("add A resource: %w", err)
		}
	}
	resp, err := b.Finish()
	if err != nil {
		return nil, fmt.Errorf("finish response: %w", err)
	}
	return resp, nil
}

// empty answers a question with no records: NXDOMAIN (RCodeNameError) for a
// name nobody has, NOERROR for a name that has no record of the asked type.
func (s *Server) empty(id uint16, q dnsmessage.Question, rcode dnsmessage.RCode) ([]byte, error) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 id,
		Response:           true,
		Authoritative:      rcode == dnsmessage.RCodeSuccess,
		RecursionDesired:   true,
		RecursionAvailable: true,
		RCode:              rcode,
	})
	if err := b.StartQuestions(); err != nil {
		return nil, fmt.Errorf("start questions: %w", err)
	}
	if err := b.Question(q); err != nil {
		return nil, fmt.Errorf("add question: %w", err)
	}
	resp, err := b.Finish()
	if err != nil {
		return nil, fmt.Errorf("finish response: %w", err)
	}
	return resp, nil
}

// forward relays the raw query to the upstream resolver and returns its raw
// response.
func (s *Server) forward(query []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", s.upstream)
	if err != nil {
		return nil, fmt.Errorf("dial upstream: %w", err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(query); err != nil {
		return nil, fmt.Errorf("write upstream: %w", err)
	}
	// Room for the largest UDP answer: a guest that announces a bigger buffer
	// (EDNS0) gets answers over 512 bytes, and one read into a smaller buffer
	// would cut them short without the truncation flag that makes it retry.
	resp := make([]byte, maxUpstreamResponse)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, fmt.Errorf("read upstream: %w", err)
	}
	return resp[:n], nil
}
