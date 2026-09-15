package network

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestEgressQuotasAndSourceTeardown(t *testing.T) {
	p := newEgressPool()
	require.Nil(t, p.reserve("unknown"))
	var all []*nativeFlow
	for source := range 4 {
		ip := fmt.Sprint(source)
		p.activateSource(ip)
		for range 64 {
			f := p.reserve(ip)
			require.NotNil(t, f)
			all = append(all, f)
		}
		require.Nil(t, p.reserve(ip))
	}
	p.activateSource("extra")
	require.Nil(t, p.reserve("extra"))
	a, b := net.Pipe()
	defer b.Close()
	require.True(t, all[0].add(a))
	p.closeSource("0")
	require.Nil(t, p.reserve("0"))
	require.NotNil(t, p.reserve("extra"))
	b.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	_, err := b.Read(buf[:])
	require.Error(t, err)
	require.Len(t, p.flows, 193)
	p.closeSource("")
	require.Empty(t, p.flows)
	require.Empty(t, p.counts)
	require.Nil(t, p.reserve("extra"))
	c, d := net.Pipe()
	defer d.Close()
	require.False(t, all[0].add(c))
}
func TestEgressConcurrentReservationAndClose(t *testing.T) {
	p := newEgressPool()
	p.activateSource("vm")
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if f := p.reserve("vm"); f != nil {
				f.close()
			}
		}()
	}
	p.closeSource("vm")
	wg.Wait()
	require.Empty(t, p.flows)
	require.Empty(t, p.counts)
}

func TestPublishedConnectionClosedWhenVMRemoved(t *testing.T) {
	n, err := NewNativeNetworkWithPolicy("172.29.0.0/24", "172.29.0.1", nil, func(string, string, uint16, bool) bool { return false })
	require.NoError(t, err)
	defer n.Close()
	n.egress.activateSource("172.29.0.2")
	require.NoError(t, n.Expose("tcp", "127.0.0.1:0", "172.29.0.2:8080"))
	listener := n.published["tcp/127.0.0.1:0"].listener.(net.Listener)
	host, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer host.Close()
	require.Eventually(t, func() bool { n.egress.mu.Lock(); defer n.egress.mu.Unlock(); return len(n.egress.flows) == 1 }, time.Second, time.Millisecond)
	n.egress.closeSource("172.29.0.2")
	host.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	_, err = host.Read(buf[:])
	require.Error(t, err)
	if e, ok := err.(net.Error); ok {
		require.False(t, e.Timeout(), "accepted host connection leaked after VM removal")
	}
	require.NoError(t, n.Unexpose("tcp", "127.0.0.1:0"))
}

func TestOwnedTCPPreservesHalfClose(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	client, err := net.Dial("tcp4", ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	left, err := ln.Accept()
	require.NoError(t, err)
	remote, err := net.Dial("tcp4", ln.Addr().String())
	require.NoError(t, err)
	defer remote.Close()
	right, err := ln.Accept()
	require.NoError(t, err)
	for _, c := range []net.Conn{client, left, remote, right} {
		require.NoError(t, c.SetDeadline(time.Now().Add(3*time.Second)))
	}
	p := newEgressPool()
	p.activateSource("vm")
	f := p.reserve("vm")
	require.True(t, f.add(left))
	require.True(t, f.add(right))
	defer f.close()
	done := make(chan struct{})
	go func() { f.duplex(left, right); close(done) }()
	_, err = client.Write([]byte("request"))
	require.NoError(t, err)
	require.NoError(t, client.(*net.TCPConn).CloseWrite())
	request, err := io.ReadAll(remote)
	require.NoError(t, err)
	require.Equal(t, "request", string(request))
	_, err = remote.Write([]byte("final response"))
	require.NoError(t, err)
	require.NoError(t, remote.(*net.TCPConn).CloseWrite())
	reply, err := io.ReadAll(client)
	require.NoError(t, err)
	require.Equal(t, "final response", string(reply))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("proxy did not release completed halves")
	}
}
