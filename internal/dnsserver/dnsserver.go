//go:build linux

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
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/AitorConS/jerboa/internal/scheduler"
)

// upstreamTimeout bounds a single forwarded query to an external resolver.
const upstreamTimeout = 3 * time.Second

// Server answers guest DNS queries from the daemon's in-memory VM state.
type Server struct {
	resolver *scheduler.Resolver
	// upstream is the "host:port" of a fallback resolver for names the daemon
	// does not own. Empty disables forwarding (unknown names return NXDOMAIN).
	upstream string
	conn     *net.UDPConn
}

// New creates a Server backed by resolver, forwarding unknown names to upstream
// (e.g. "1.1.1.1:53"). Pass an empty upstream to disable forwarding.
func New(resolver *scheduler.Resolver, upstream string) *Server {
	return &Server{resolver: resolver, upstream: upstream}
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
	s.conn = conn
	slog.Info("guest dns server listening", "addr", addr)

	buf := make([]byte, 512)
	for {
		n, src, readErr := conn.ReadFromUDP(buf)
		if readErr != nil {
			// A closed connection ends the loop cleanly.
			if strings.Contains(readErr.Error(), "use of closed network connection") {
				return nil
			}
			slog.Debug("dnsserver: read", "err", readErr)
			continue
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		go s.handle(query, src)
	}
}

// Close stops the server.
func (s *Server) Close() error {
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

func (s *Server) handle(query []byte, src *net.UDPAddr) {
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
	if _, err := s.conn.WriteToUDP(resp, src); err != nil {
		slog.Debug("dnsserver: write", "src", src.IP, "err", err)
	}
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

	if q.Type == dnsmessage.TypeA {
		name := strings.TrimSuffix(q.Name.String(), ".")
		network := s.resolver.NetworkForIP(srcIP)
		if rec, rErr := s.resolver.Resolve(name, network); rErr == nil {
			if ip := net.ParseIP(rec.IP).To4(); ip != nil {
				return s.answerA(hdr.ID, q, [4]byte{ip[0], ip[1], ip[2], ip[3]})
			}
		}
	}

	// Not an A record we own (or an AAAA/other query): forward upstream so
	// ordinary internet name resolution still works for the guest.
	if s.upstream != "" {
		if resp, fErr := s.forward(query); fErr == nil {
			return resp, nil
		}
	}
	return s.nxdomain(hdr.ID, q)
}

func (s *Server) answerA(id uint16, q dnsmessage.Question, ip [4]byte) ([]byte, error) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 id,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   true,
		RecursionAvailable: true,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	err := b.AResource(dnsmessage.ResourceHeader{
		Name:  q.Name,
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
		TTL:   30,
	}, dnsmessage.AResource{A: ip})
	if err != nil {
		return nil, err
	}
	return b.Finish()
}

func (s *Server) nxdomain(id uint16, q dnsmessage.Question) ([]byte, error) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 id,
		Response:           true,
		RecursionDesired:   true,
		RecursionAvailable: true,
		RCode:              dnsmessage.RCodeNameError,
	})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	return b.Finish()
}

// forward relays the raw query to the upstream resolver and returns its raw
// response.
func (s *Server) forward(query []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", s.upstream)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, err
	}
	return resp[:n], nil
}
