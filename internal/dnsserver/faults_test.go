//go:build linux || (darwin && arm64)

package dnsserver

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/scheduler"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// What must hold when the guest DNS server is sent packets no resolver would
// send, when its upstream resolver is down, silent or oversized, and when it
// is flooded: it never crashes, never answers a packet it could not read,
// never leaks a network's names to another network or to the upstream, and
// keeps answering the next well-formed question.

const clientIP = "172.25.0.9"

func appGuests() vms {
	return vms{
		{ID: "client", State: vm.StateRunning, Cfg: vm.Config{Name: "client", NetworkName: "app", IPAddress: clientIP}},
		{ID: "pg", State: vm.StateRunning, Cfg: vm.Config{Name: "postgres", NetworkName: "app", IPAddress: "172.25.0.2", NetworkAliases: []string{"db.internal"}}},
		{ID: "old", State: vm.StateStopped, Cfg: vm.Config{Name: "retired", NetworkName: "app", IPAddress: "172.25.0.3"}},
		{ID: "boot", State: vm.StateStarting, Cfg: vm.Config{Name: "booting", NetworkName: "app", IPAddress: "172.25.0.4"}},
		{ID: "v6", State: vm.StateRunning, Cfg: vm.Config{Name: "sixonly", NetworkName: "app", IPAddress: "fd00::5"}},
		{ID: "bad", State: vm.StateRunning, Cfg: vm.Config{Name: "broken", NetworkName: "app", IPAddress: "not-an-address"}},
		{ID: "secret", State: vm.StateRunning, Cfg: vm.Config{Name: "vault", NetworkName: "other", IPAddress: "172.26.0.2"}},
		nil,
	}
}

// deadUpstream is a loopback UDP address nobody listens on.
func deadUpstream(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	addr := conn.LocalAddr().String()
	require.NoError(t, conn.Close())
	return addr
}

// scriptedUpstream answers every query with whatever reply builds from it and
// counts the queries; a nil reply stays silent.
func scriptedUpstream(t *testing.T, reply func(query []byte) []byte) (addr string, asked *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	asked = &atomic.Int32{}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			asked.Add(1)
			if out := reply(append([]byte(nil), buf[:n]...)); out != nil {
				_, _ = conn.WriteToUDP(out, from)
			}
		}
	}()
	return conn.LocalAddr().String(), asked
}

func mustAnswer(t *testing.T, s *Server, query []byte, src string) (dnsmessage.Header, []dnsmessage.Resource) {
	t.Helper()
	resp, err := s.Answer(query, src)
	require.NoError(t, err)
	return parse(t, resp)
}

// A packet cut short anywhere is not a question: it gets no answer, and it is
// never passed on to the upstream resolver.
func TestFaults_TruncatedQueryAtEveryLength(t *testing.T) {
	addr, asked := upstream(t)
	s := New(scheduler.NewResolver(appGuests()), addr)
	for _, name := range []string{"postgres", "example.com"} {
		full := question(t, name, dnsmessage.TypeA)
		for size := range len(full) {
			resp, err := s.Answer(full[:size], clientIP)
			require.Error(t, err, "%s cut to %d of %d bytes", name, size, len(full))
			require.Nil(t, resp)
		}
	}
	require.Zero(t, asked.Load(), "a packet that cannot be read must not leave the host")
}

// header builds the 12 fixed bytes of a DNS message.
func header(id, flags, questions uint16) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[2:], flags)
	binary.BigEndian.PutUint16(b[4:], questions)
	return b
}

func malformedQueries() map[string][]byte {
	tail := []byte{0, 1, 0, 1} // type A, class IN
	long := append([]byte{}, header(1, 0x0100, 1)...)
	for range 30 { // 30 labels of 9 bytes: a name over the 255-byte limit
		long = append(long, 8, 'a', 'a', 'a', 'a', 'a', 'a', 'a', 'a')
	}
	long = append(append(long, 0), tail...)
	return map[string][]byte{
		"empty":                    {},
		"one byte":                 {0x42},
		"header only":              header(1, 0x0100, 0),
		"question count lies":      header(1, 0x0100, 1),
		"65535 questions, none":    header(1, 0x0100, 0xffff),
		"pointer to itself":        append(append(header(1, 0x0100, 1), 0xc0, 12), tail...),
		"pointer loop":             append(append(header(1, 0x0100, 1), 0xc0, 14, 0xc0, 12), tail...),
		"pointer past the end":     append(append(header(1, 0x0100, 1), 0xc0, 0xff), tail...),
		"label longer than packet": append(header(1, 0x0100, 1), 63, 'a', 'b'),
		"reserved label type":      append(append(header(1, 0x0100, 1), 0x80, 'a', 0), tail...),
		"name without end":         append(header(1, 0x0100, 1), 3, 'a', 'b', 'c'),
		"name too long":            long,
		"missing class":            append(header(1, 0x0100, 1), 1, 'a', 0, 0, 1),
		"all ones":                 bytes.Repeat([]byte{0xff}, 64),
		"all zeroes":               make([]byte, 11),
		"text":                     []byte("GET / HTTP/1.1\r\nHost: db\r\n\r\n"),
	}
}

func TestFaults_MalformedQueriesGetNoAnswer(t *testing.T) {
	addr, asked := upstream(t)
	s := New(scheduler.NewResolver(appGuests()), addr)
	for name, query := range malformedQueries() {
		for _, src := range []string{clientIP, "10.9.9.9", "", "not-an-ip"} {
			resp, err := s.Answer(query, src)
			require.Error(t, err, name)
			require.Nil(t, resp, name)
		}
	}
	require.Zero(t, asked.Load(), "a packet that cannot be read must not leave the host")
}

// Odd but readable questions are answered, not dropped: the answer carries the
// asker's ID and question back, which is how a resolver matches it.
func TestFaults_UnusualQuestionsAreAnsweredInKind(t *testing.T) {
	s := New(scheduler.NewResolver(appGuests()), "")
	build := func(id uint16, name string, typ dnsmessage.Type, class dnsmessage.Class, extra int) []byte {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id})
		require.NoError(t, b.StartQuestions())
		for range 1 + extra {
			require.NoError(t, b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: class}))
		}
		out, err := b.Finish()
		require.NoError(t, err)
		return out
	}
	longest := strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("b", 61) + "."
	cases := map[string][]byte{
		"root name":         build(0xffff, ".", dnsmessage.TypeA, dnsmessage.ClassINET, 0),
		"longest name":      build(2, longest, dnsmessage.TypeA, dnsmessage.ClassINET, 0),
		"chaos class":       build(3, "version.bind.", dnsmessage.TypeTXT, dnsmessage.ClassCHAOS, 0),
		"any type":          build(4, "nobody.", dnsmessage.TypeALL, dnsmessage.ClassANY, 0),
		"type zero":         build(5, "nobody.", dnsmessage.Type(0), dnsmessage.ClassINET, 0),
		"two questions":     build(6, "nobody.", dnsmessage.TypeA, dnsmessage.ClassINET, 1),
		"trailing garbage":  append(build(7, "nobody.", dnsmessage.TypeA, dnsmessage.ClassINET, 0), 0xde, 0xad, 0xbe, 0xef),
		"bytes in the name": build(8, "\x00\xff\x7f.", dnsmessage.TypeA, dnsmessage.ClassINET, 0),
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			resp, err := s.Answer(query, clientIP)
			require.NoError(t, err)
			var p dnsmessage.Parser
			hdr, err := p.Start(resp)
			require.NoError(t, err)
			require.Equal(t, binary.BigEndian.Uint16(query), hdr.ID)
			require.True(t, hdr.Response)
			require.Equal(t, dnsmessage.RCodeNameError, hdr.RCode)

			var asked dnsmessage.Parser
			_, err = asked.Start(query)
			require.NoError(t, err)
			want, err := asked.Question()
			require.NoError(t, err)
			got, err := p.Question()
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

// Names that exist but must not be told: a VM that is stopped or not yet
// ready, a VM in another network, and addresses that are no IPv4 address. With
// no upstream they are all NXDOMAIN; none of them makes the server fail.
func TestFaults_NamesThatMustNotResolve(t *testing.T) {
	s := New(scheduler.NewResolver(appGuests()), "")
	for _, name := range []string{"retired", "booting", "vault", "vault.other", "sixonly", "broken", "nobody", "POSTGRES.example"} {
		hdr, answers := mustAnswer(t, s, question(t, name, dnsmessage.TypeA), clientIP)
		require.Equal(t, dnsmessage.RCodeNameError, hdr.RCode, name)
		require.Empty(t, answers, name)
		require.False(t, hdr.Authoritative, name)
	}

	// A caller the daemon does not know (a spoofed or stale source address)
	// owns no names, whatever it asks for.
	for _, src := range []string{"172.26.0.99", "172.25.0.3", "127.0.0.1", "", "garbage", "::1"} {
		hdr, answers := mustAnswer(t, s, question(t, "postgres", dnsmessage.TypeA), src)
		require.Equal(t, dnsmessage.RCodeNameError, hdr.RCode, "source %q", src)
		require.Empty(t, answers, "source %q", src)
	}

	// A guest that is still starting already resolves its network's names.
	hdr, answers := mustAnswer(t, s, question(t, "postgres", dnsmessage.TypeA), "172.25.0.4")
	require.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode)
	require.Len(t, answers, 1)
}

// Two running VMs answer to one alias, as the replicas of a service do.
func TestFaults_SharedAliasResolves(t *testing.T) {
	addr, asked := upstream(t)
	guests := vms{
		{ID: "client", State: vm.StateRunning, Cfg: vm.Config{Name: "client", NetworkName: "app", IPAddress: clientIP}},
		{ID: "w1", State: vm.StateRunning, Cfg: vm.Config{Name: "web-1", NetworkName: "app", IPAddress: "172.25.0.11", NetworkAliases: []string{"web"}}},
		{ID: "w2", State: vm.StateRunning, Cfg: vm.Config{Name: "web-2", NetworkName: "app", IPAddress: "172.25.0.12", NetworkAliases: []string{"web"}}},
	}
	s := New(scheduler.NewResolver(guests), addr)
	first := map[string]int{}
	for range 6 {
		hdr, answers := mustAnswer(t, s, question(t, "web", dnsmessage.TypeA), clientIP)
		require.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode)
		require.True(t, hdr.Authoritative)
		require.Len(t, answers, 2, "both replicas answer to the alias")
		got := map[string]bool{}
		for i, a := range answers {
			res, ok := a.Body.(*dnsmessage.AResource)
			require.True(t, ok)
			ip := net.IP(res.A[:]).String()
			got[ip] = true
			if i == 0 {
				first[ip]++
			}
		}
		require.Equal(t, map[string]bool{"172.25.0.11": true, "172.25.0.12": true}, got)
	}
	require.Zero(t, asked.Load(), "a private name was sent to the upstream resolver")
	// The order rotates, so clients that take the first address spread out.
	require.Equal(t, map[string]int{"172.25.0.11": 3, "172.25.0.12": 3}, first)

	// A name with no IPv4 record of its own stays here too.
	hdr, answers := mustAnswer(t, s, question(t, "web", dnsmessage.TypeAAAA), clientIP)
	require.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode)
	require.Empty(t, answers)
	require.Zero(t, asked.Load())
}

// More replicas than one UDP answer holds: the answer stays within what any
// resolver accepts, and every replica is reached over successive questions.
func TestFaults_SharedAliasWithManyReplicas(t *testing.T) {
	guests := vms{{ID: "client", State: vm.StateRunning, Cfg: vm.Config{Name: "client", NetworkName: "app", IPAddress: clientIP}}}
	const replicas = 40
	for i := range replicas {
		guests = append(guests, &vm.VM{ID: fmt.Sprintf("w%d", i), State: vm.StateRunning, Cfg: vm.Config{
			Name: fmt.Sprintf("web-%d", i), NetworkName: "app", IPAddress: fmt.Sprintf("172.25.1.%d", i+1), NetworkAliases: []string{"web"}}})
	}
	s := New(scheduler.NewResolver(guests), "")
	seen := map[string]bool{}
	for range replicas {
		resp, err := s.buildResponse(question(t, "web", dnsmessage.TypeA), clientIP)
		require.NoError(t, err)
		require.LessOrEqual(t, len(resp), 512)
		_, answers := mustAnswer(t, s, question(t, "web", dnsmessage.TypeA), clientIP)
		require.Len(t, answers, maxAnswers)
		for _, a := range answers {
			res, ok := a.Body.(*dnsmessage.AResource)
			require.True(t, ok)
			seen[net.IP(res.A[:]).String()] = true
		}
	}
	require.Len(t, seen, replicas)
}

// The upstream resolver is unreachable in every way it can be: the guest gets
// a prompt NXDOMAIN for foreign names, and names of its own network still
// resolve because they never depended on the upstream.
func TestFaults_UpstreamUnreachable(t *testing.T) {
	for name, addr := range map[string]string{
		"nobody listening":   deadUpstream(t),
		"not an address":     "this is not an address",
		"unresolvable host":  "upstream.invalid:53",
		"port out of range":  "127.0.0.1:99999",
		"missing port":       "127.0.0.1",
		"forwarding disable": "",
	} {
		t.Run(name, func(t *testing.T) {
			s := New(scheduler.NewResolver(appGuests()), addr)
			began := time.Now()
			hdr, answers := mustAnswer(t, s, question(t, "example.com", dnsmessage.TypeA), clientIP)
			require.Equal(t, dnsmessage.RCodeNameError, hdr.RCode)
			require.Equal(t, uint16(7), hdr.ID)
			require.Empty(t, answers)
			require.Less(t, time.Since(began), upstreamTimeout, "a dead upstream must fail fast, not time out")

			hdr, answers = mustAnswer(t, s, question(t, "postgres", dnsmessage.TypeA), clientIP)
			require.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode)
			require.Len(t, answers, 1)
		})
	}
}

// An answer larger than the classic 512 bytes (a guest that announced a
// bigger buffer with EDNS0 gets those) must be relayed whole: cut short, the
// guest's resolver receives a message it cannot parse.
func TestFaults_LargeUpstreamAnswerIsRelayedWhole(t *testing.T) {
	var sent atomic.Pointer[[]byte]
	addr, _ := scriptedUpstream(t, func(query []byte) []byte {
		var p dnsmessage.Parser
		hdr, _ := p.Start(query)
		q, _ := p.Question()
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: hdr.ID, Response: true})
		_ = b.StartQuestions()
		_ = b.Question(q)
		_ = b.StartAnswers()
		for range 12 {
			_ = b.TXTResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 60},
				dnsmessage.TXTResource{TXT: []string{strings.Repeat("k", 200)}})
		}
		out, _ := b.Finish()
		sent.Store(&out)
		return out
	})
	s := New(scheduler.NewResolver(appGuests()), addr)
	resp, err := s.Answer(question(t, "example.com", dnsmessage.TypeTXT), clientIP)
	require.NoError(t, err)
	require.Greater(t, len(*sent.Load()), 2048)
	require.Equal(t, *sent.Load(), resp)
	_, answers := parse(t, resp)
	require.Len(t, answers, 12)
}

// serving starts the server on a loopback port and returns a socket connected
// to it, once the server answers.
func serving(t *testing.T, s *Server) *net.UDPConn {
	t.Helper()
	addr := deadUpstream(t) // a free loopback port
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(addr) }()
	t.Cleanup(func() {
		_ = s.Close()
		select {
		case err := <-done:
			assert.NoError(t, err, "closing the server ends ListenAndServe cleanly")
		case <-time.After(10 * time.Second):
			t.Error("ListenAndServe did not return after Close")
		}
	})
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	require.NoError(t, err)
	conn, err := net.DialUDP("udp", nil, udpAddr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	// Wait for the socket to be bound; from then on the kernel queues what
	// is sent to it. No probe question is asked: with a slow upstream its
	// late answer would be taken for the answer to the test's own question.
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.conn != nil
	}, 10*time.Second, time.Millisecond, "the server never listened")
	return conn
}

// exchange sends one packet and waits up to wait for the answer to it.
func exchange(t *testing.T, conn *net.UDPConn, query []byte, wait time.Duration) ([]byte, bool) {
	t.Helper()
	_, err := conn.Write(query)
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(wait)))
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, false
	}
	return buf[:n], true
}

// Over a real socket: garbage, cut and oversized packets get no reply at all
// (a reply to junk is how a server becomes a reflector) and the server goes
// on answering.
func TestFaults_ServerSurvivesJunkPackets(t *testing.T) {
	s := New(scheduler.NewResolver(appGuests()), "")
	conn := serving(t, s)

	junk := malformedQueries()
	delete(junk, "empty") // a zero-length datagram is not sent by every stack
	junk["oversized"] = bytes.Repeat([]byte{0xab}, 4000)
	whole := question(t, "example.com", dnsmessage.TypeA)
	junk["cut in half"] = whole[:len(whole)/2]
	for _, packet := range junk {
		_, err := conn.Write(packet)
		require.NoError(t, err)
	}

	// The first datagram to come back must be the answer to this question:
	// had any junk been answered, its reply would be read here instead.
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0xbeef})
	require.NoError(t, b.StartQuestions())
	require.NoError(t, b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("survivor."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}))
	query, err := b.Finish()
	require.NoError(t, err)
	resp, ok := exchange(t, conn, query, 10*time.Second)
	require.True(t, ok, "the server stopped answering after junk")
	hdr, _ := parse(t, resp)
	require.Equal(t, uint16(0xbeef), hdr.ID)
	require.Equal(t, dnsmessage.RCodeNameError, hdr.RCode)
}

// The upstream resolver takes the question and never answers. Meanwhile the
// server keeps answering other questions, and the one that hung ends with
// NXDOMAIN once the upstream's time is up instead of staying unanswered.
func TestFaults_SilentUpstreamDoesNotBlockOtherQueries(t *testing.T) {
	t.Parallel()
	addr, asked := scriptedUpstream(t, func([]byte) []byte { return nil })
	s := New(scheduler.NewResolver(appGuests()), addr)
	conn := serving(t, s)
	asked.Store(0)

	hung := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1111})
	require.NoError(t, hung.StartQuestions())
	require.NoError(t, hung.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("slow.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}))
	query, err := hung.Finish()
	require.NoError(t, err)
	began := time.Now()
	_, err = conn.Write(query)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return asked.Load() == 1 }, 10*time.Second, 5*time.Millisecond)

	// A second client is served while the first question is still out.
	other, err := net.DialUDP("udp", nil, conn.RemoteAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer other.Close()
	_, err = other.Write(query)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return asked.Load() == 2 }, 10*time.Second, 5*time.Millisecond,
		"a question waiting for the upstream must not hold the next one")
	require.Less(t, time.Since(began), upstreamTimeout, "the second question was only handled after the first timed out")

	resp, ok := exchange(t, conn, nil, upstreamTimeout+10*time.Second)
	require.True(t, ok, "a question the upstream ignored was never answered")
	hdr, _ := parse(t, resp)
	require.Equal(t, uint16(0x1111), hdr.ID)
	require.Equal(t, dnsmessage.RCodeNameError, hdr.RCode)
}

// Every worker is busy (a flood, or a stuck upstream): further packets are
// dropped rather than queued without bound, and service resumes as soon as a
// worker is free.
func TestFaults_SaturatedServerDropsThenRecovers(t *testing.T) {
	s := New(scheduler.NewResolver(appGuests()), "")
	conn := serving(t, s)

	for range maxConcurrentQueries {
		s.sem <- struct{}{}
	}
	_, answered := exchange(t, conn, question(t, "dropped", dnsmessage.TypeA), 150*time.Millisecond)
	require.False(t, answered, "a saturated server must drop, not queue")
	for range maxConcurrentQueries {
		<-s.sem
	}

	resp, ok := exchange(t, conn, question(t, "postgres", dnsmessage.TypeA), 10*time.Second)
	require.True(t, ok, "the server did not recover from saturation")
	hdr, _ := parse(t, resp)
	require.Equal(t, uint16(7), hdr.ID)
	require.Len(t, s.sem, 0) //nolint:testifylint // a channel's length, not a collection's
}

// Many guests asking at once all get their own answer back.
func TestFaults_ConcurrentQueries(t *testing.T) {
	s := New(scheduler.NewResolver(appGuests()), "")
	first := serving(t, s)
	server := first.RemoteAddr().(*net.UDPAddr)

	const clients = 16
	errs := make(chan string, clients)
	for i := range clients {
		go func() {
			conn, err := net.DialUDP("udp", nil, server)
			if err != nil {
				errs <- err.Error()
				return
			}
			defer conn.Close()
			id := uint16(1000 + i)
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id})
			_ = b.StartQuestions()
			_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("nobody."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
			query, _ := b.Finish()
			for range 20 {
				_, _ = conn.Write(query)
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				buf := make([]byte, 512)
				n, err := conn.Read(buf)
				if err != nil {
					errs <- "no answer: " + err.Error()
					return
				}
				if n < 2 || binary.BigEndian.Uint16(buf) != id {
					errs <- "an answer meant for another client"
					return
				}
			}
			errs <- ""
		}()
	}
	for range clients {
		require.Empty(t, <-errs)
	}
}

// The address cannot be bound: ListenAndServe says so instead of serving
// nothing, and a server that never listened closes without error.
func TestFaults_ListenFailures(t *testing.T) {
	s := New(scheduler.NewResolver(appGuests()), "")
	require.NoError(t, s.Close(), "closing a server that never listened")
	require.ErrorContains(t, s.ListenAndServe("not an address"), "resolve")
	require.ErrorContains(t, s.ListenAndServe("127.0.0.1:99999"), "resolve")

	taken, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer taken.Close()
	err = s.ListenAndServe(taken.LocalAddr().String())
	require.ErrorContains(t, err, "listen", "a port another process holds")
	require.NoError(t, s.Close())
}

// Any bytes at all, from a guest of the network or from a stranger: the
// server does not panic, and what it sends back is a well-formed response to
// the question it was asked.
func FuzzAnswer(f *testing.F) {
	t := &testing.T{}
	for _, name := range []string{"postgres", "db.internal", "postgres.app", "example.com", "vault"} {
		for _, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA, dnsmessage.TypeTXT} {
			f.Add(question(t, name, typ), true)
		}
	}
	for _, query := range malformedQueries() {
		f.Add(query, true)
		f.Add(query, false)
	}
	s := New(scheduler.NewResolver(appGuests()), "")
	f.Fuzz(func(t *testing.T, query []byte, known bool) {
		src := "203.0.113.7"
		if known {
			src = clientIP
		}
		resp, err := s.Answer(query, src)
		if err != nil {
			if resp != nil {
				t.Fatalf("an error came with %d bytes of response", len(resp))
			}
			return
		}
		var p dnsmessage.Parser
		hdr, err := p.Start(resp)
		if err != nil {
			t.Fatalf("response does not parse: %v", err)
		}
		if !hdr.Response || hdr.ID != binary.BigEndian.Uint16(query) {
			t.Fatalf("response header %+v does not answer query ID %d", hdr, binary.BigEndian.Uint16(query))
		}
		if _, err := p.Question(); err != nil {
			t.Fatalf("response carries no question: %v", err)
		}
		if err := p.SkipAllQuestions(); err != nil {
			t.Fatalf("response questions do not parse: %v", err)
		}
		answers, err := p.AllAnswers()
		if err != nil {
			t.Fatalf("response answers do not parse: %v", err)
		}
		if !known && len(answers) > 0 {
			t.Fatalf("a caller outside every network was told %d records", len(answers))
		}
	})
}
