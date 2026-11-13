package dnsserver

import (
	"net"
	"sync/atomic"
	"testing"

	"github.com/AitorConS/jerboa/internal/scheduler"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

type vms []*vm.VM

func (v vms) List() []*vm.VM { return v }

func question(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	require.NoError(t, b.StartQuestions())
	require.NoError(t, b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: typ, Class: dnsmessage.ClassINET}))
	out, err := b.Finish()
	require.NoError(t, err)
	return out
}

// upstream is a resolver that answers every question with an address, as a
// public domain of the same name would, and counts what it is asked.
func upstream(t *testing.T) (addr string, asked *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	asked = &atomic.Int32{}
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			asked.Add(1)
			var p dnsmessage.Parser
			hdr, _ := p.Start(buf[:n])
			q, _ := p.Question()
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: hdr.ID, Response: true})
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			_ = b.AAAAResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 0x01}})
			out, _ := b.Finish()
			_, _ = conn.WriteToUDP(out, from)
		}
	}()
	return conn.LocalAddr().String(), asked
}

func parse(t *testing.T, resp []byte) (dnsmessage.Header, []dnsmessage.Resource) {
	t.Helper()
	var p dnsmessage.Parser
	hdr, err := p.Start(resp)
	require.NoError(t, err)
	require.NoError(t, p.SkipAllQuestions())
	answers, err := p.AllAnswers()
	require.NoError(t, err)
	return hdr, answers
}

func TestNamesOfTheNetworkNeverReachTheUpstreamResolver(t *testing.T) {
	addr, asked := upstream(t)
	guests := vms{
		{ID: "client", State: vm.StateRunning, Cfg: vm.Config{Name: "client", NetworkName: "app", IPAddress: "172.25.0.9"}},
		{ID: "pg", State: vm.StateRunning, Cfg: vm.Config{Name: "postgres", NetworkName: "app", IPAddress: "172.25.0.2", NetworkAliases: []string{"db.internal"}}},
	}
	s := New(scheduler.NewResolver(guests), addr)

	for _, name := range []string{"postgres", "postgres.app", "db.internal"} {
		resp, err := s.Answer(question(t, name, dnsmessage.TypeA), "172.25.0.9")
		require.NoError(t, err)
		hdr, answers := parse(t, resp)
		require.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode, name)
		require.Len(t, answers, 1, name)
		require.Equal(t, [4]byte{172, 25, 0, 2}, answers[0].Body.(*dnsmessage.AResource).A, name)

		// A public domain of the same name must not answer for it.
		for _, typ := range []dnsmessage.Type{dnsmessage.TypeAAAA, dnsmessage.TypeMX, dnsmessage.TypeTXT} {
			resp, err = s.Answer(question(t, name, typ), "172.25.0.9")
			require.NoError(t, err)
			hdr, answers = parse(t, resp)
			require.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode, name)
			require.Empty(t, answers, name)
		}
	}
	require.Zero(t, asked.Load(), "no question about a name of the network leaves the host")

	// Every other name is the upstream resolver's to answer.
	resp, err := s.Answer(question(t, "example.com", dnsmessage.TypeAAAA), "172.25.0.9")
	require.NoError(t, err)
	_, answers := parse(t, resp)
	require.Len(t, answers, 1)
	require.EqualValues(t, 1, asked.Load())

	// A caller that is in no network owns no names.
	_, err = s.Answer(question(t, "postgres", dnsmessage.TypeAAAA), "10.9.9.9")
	require.NoError(t, err)
	require.EqualValues(t, 2, asked.Load())
}
