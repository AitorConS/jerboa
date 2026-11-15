package network

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What must hold for a published port when one of the three parties
// misbehaves: the guest refuses, dies or stops reading, the client stalls or
// vanishes, the VM is stopped with connections open. A broken connection
// costs only itself: the port keeps serving, nothing hangs, nothing leaks.

// wait bounds every blocking step of these tests. It is generous: it is only
// reached when the behavior under test is broken.
const wait = 10 * time.Second

// backend is a guest service: a loopback listener that hands every accepted
// connection to handle.
type backend struct {
	ln       net.Listener
	accepted atomic.Int64
	wg       sync.WaitGroup
}

func startBackend(t *testing.T, handle func(c *net.TCPConn)) *backend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return serveBackend(t, ln, handle)
}

func serveBackend(t *testing.T, ln net.Listener, handle func(c *net.TCPConn)) *backend {
	t.Helper()
	b := &backend{ln: ln}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.accepted.Add(1)
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				defer c.Close()
				handle(c.(*net.TCPConn))
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return b
}

func (b *backend) port() uint16 { return uint16(b.ln.Addr().(*net.TCPAddr).Port) }

// done waits until the listener and every handler returned.
func (b *backend) done(t *testing.T) {
	t.Helper()
	_ = b.ln.Close()
	finished := make(chan struct{})
	go func() { b.wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(wait):
		t.Fatal("a guest connection was left open")
	}
}

// publish forwards a fresh loopback port to guestPort and returns the
// forwarder and the published address.
func publish(t *testing.T, guestPort uint16) (*Forwarder, string) {
	t.Helper()
	// The kernel may hand out the port a test has just released for its
	// guest; a forwarder listening on its own target would dial itself.
	hostPort := freeTCPPort(t)
	for hostPort == guestPort {
		hostPort = freeTCPPort(t)
	}
	f, err := StartForwarder("127.0.0.1", []PortForward{{HostPort: hostPort, GuestPort: guestPort, Protocol: "tcp", BindAddr: "127.0.0.1"}})
	require.NoError(t, err)
	t.Cleanup(f.Close)
	return f, fmt.Sprintf("127.0.0.1:%d", hostPort)
}

func dial(t *testing.T, addr string) *net.TCPConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, wait)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.SetDeadline(time.Now().Add(wait)))
	return c.(*net.TCPConn)
}

// closeWithin fails the test when Close does not return: a forwarder that
// hangs there keeps the VM from stopping.
func closeWithin(t *testing.T, f *Forwarder) {
	t.Helper()
	closed := make(chan struct{})
	go func() { f.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(wait):
		t.Fatal("Forwarder.Close did not return")
	}
}

func tracked(f *Forwarder) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.connections)
}

// requireEnded reads until the connection ends and fails when it is still
// open at the deadline: the forwarder left the client hanging.
func requireEnded(t *testing.T, c net.Conn) {
	t.Helper()
	_, err := io.Copy(io.Discard, c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the connection was left open")
	}
}

func TestForwarderFaults_GuestRefusesThenComesUp(t *testing.T) {
	// The guest has not opened its port yet (it is booting): clients are
	// dropped at once instead of hanging, and the port works as soon as the
	// guest listens, without republishing.
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	guestAddr := reserve.Addr().String()
	guestPort := uint16(reserve.Addr().(*net.TCPAddr).Port)
	require.NoError(t, reserve.Close())

	f, addr := publish(t, guestPort)
	for range 5 {
		c := dial(t, addr)
		_, _ = c.Write([]byte("anyone there?"))
		requireEnded(t, c)
	}
	require.Eventually(t, func() bool { return tracked(f) == 0 }, wait, 5*time.Millisecond, "refused connections stayed tracked")

	ln, err := net.Listen("tcp", guestAddr)
	if err != nil {
		t.Skipf("the guest port was taken meanwhile: %v", err)
	}
	b := serveBackend(t, ln, func(c *net.TCPConn) { _, _ = io.Copy(c, c); _ = c.CloseWrite() })
	c := dial(t, addr)
	_, err = c.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, c.CloseWrite())
	got, err := io.ReadAll(c)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
	assert.EqualValues(t, 1, b.accepted.Load())
}

func TestForwarderFaults_GuestClosesMidStream(t *testing.T) {
	payload := make([]byte, 300_000)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	b := startBackend(t, func(c *net.TCPConn) {
		// The service dies after part of its answer; everything it did send
		// before closing must reach the client, followed by a clean end.
		_, _ = c.Write(payload)
	})
	f, addr := publish(t, b.port())

	c := dial(t, addr)
	got, err := io.ReadAll(c)
	require.NoError(t, err, "the guest closed cleanly, so the client must see a clean end")
	require.True(t, bytes.Equal(payload, got), "got %d of %d bytes", len(got), len(payload))
	// The client's own direction is still open until it closes it too.
	require.NoError(t, c.Close())
	require.Eventually(t, func() bool { return tracked(f) == 0 }, wait, 5*time.Millisecond)
}

func TestForwarderFaults_GuestResetsConnection(t *testing.T) {
	b := startBackend(t, func(c *net.TCPConn) {
		// The guest crashes with the request unread: its kernel answers RST.
		var one [1]byte
		_, _ = c.Read(one[:])
		_ = c.SetLinger(0)
	})
	f, addr := publish(t, b.port())

	c := dial(t, addr)
	_, err := c.Write(bytes.Repeat([]byte("x"), 4096))
	require.NoError(t, err)
	requireEnded(t, c)
	require.Eventually(t, func() bool { return tracked(f) == 0 }, wait, 5*time.Millisecond)

	// The port is unharmed.
	c2 := dial(t, addr)
	_, err = c2.Write([]byte("y"))
	require.NoError(t, err)
	requireEnded(t, c2)
	assert.EqualValues(t, 2, b.accepted.Load())
}

func TestForwarderFaults_GuestHalfClosesAndKeepsReading(t *testing.T) {
	received := make(chan []byte, 1)
	b := startBackend(t, func(c *net.TCPConn) {
		// The guest sends its whole answer first and half-closes, then reads
		// what the client still uploads.
		_, _ = c.Write([]byte("answer"))
		_ = c.CloseWrite()
		data, _ := io.ReadAll(c)
		received <- data
	})
	_, addr := publish(t, b.port())

	c := dial(t, addr)
	got, err := io.ReadAll(c)
	require.NoError(t, err)
	require.Equal(t, "answer", string(got))
	// The guest's half-close must not have closed the other direction.
	_, err = c.Write([]byte("late upload"))
	require.NoError(t, err)
	require.NoError(t, c.CloseWrite())
	select {
	case data := <-received:
		assert.Equal(t, "late upload", string(data))
	case <-time.After(wait):
		t.Fatal("the guest never saw the end of the upload")
	}
}

func TestForwarderFaults_ClientVanishes(t *testing.T) {
	for name, leave := range map[string]func(c *net.TCPConn){
		"closes":       func(c *net.TCPConn) { _ = c.Close() },
		"resets":       func(c *net.TCPConn) { _ = c.SetLinger(0); _ = c.Close() },
		"closes write": func(c *net.TCPConn) { _ = c.CloseWrite() },
	} {
		t.Run(name, func(t *testing.T) {
			ended := make(chan []byte, 1)
			b := startBackend(t, func(c *net.TCPConn) {
				data, _ := io.ReadAll(c)
				ended <- data
			})
			f, addr := publish(t, b.port())

			c := dial(t, addr)
			_, err := c.Write([]byte("partial request"))
			require.NoError(t, err)
			// Wait until the bytes crossed, so the test is about the
			// disconnection and not about data still in flight.
			require.Eventually(t, func() bool { return b.accepted.Load() == 1 }, wait, time.Millisecond)
			leave(c)

			// The guest must learn that its client is gone; a connection
			// that stays open there is a slot the service never gets back.
			select {
			case data := <-ended:
				if name != "resets" {
					assert.Equal(t, "partial request", string(data))
				}
			case <-time.After(wait):
				t.Fatal("the guest connection outlived its client")
			}
			b.done(t)
			require.Eventually(t, func() bool { return tracked(f) == 0 }, wait, 5*time.Millisecond)
		})
	}
}

func TestForwarderFaults_ClientStallsWhileGuestFloods(t *testing.T) {
	writeEnded := make(chan error, 1)
	b := startBackend(t, func(c *net.TCPConn) {
		// A guest that streams without end to a client that never reads:
		// every buffer on the way fills up and the copy blocks.
		chunk := make([]byte, 64<<10)
		for {
			_ = c.SetWriteDeadline(time.Now().Add(wait))
			if _, err := c.Write(chunk); err != nil {
				writeEnded <- err
				return
			}
		}
	})
	f, addr := publish(t, b.port())
	c := dial(t, addr)
	_, err := c.Write([]byte("go"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return b.accepted.Load() == 1 && tracked(f) == 1 }, wait, time.Millisecond)

	// Stopping the VM must not wait for the stalled client.
	closeWithin(t, f)
	select {
	case err := <-writeEnded:
		var ne net.Error
		require.False(t, errors.As(err, &ne) && ne.Timeout(), "the guest side was left open after Close")
	case <-time.After(2 * wait):
		t.Fatal("the guest side was left open after Close")
	}
	requireEnded(t, c)
	assert.Zero(t, tracked(f))
}

func TestForwarderFaults_CloseWithConnectionsInEveryState(t *testing.T) {
	var guestOpen atomic.Int64
	b := startBackend(t, func(c *net.TCPConn) {
		guestOpen.Add(1)
		defer guestOpen.Add(-1)
		_, _ = io.Copy(c, c)
	})
	f, addr := publish(t, b.port())

	const clients = 40
	conns := make([]*net.TCPConn, clients)
	for i := range conns {
		conns[i] = dial(t, addr)
		switch i % 4 {
		case 0: // idle, nothing sent yet
		case 1: // mid-request
			_, err := conns[i].Write([]byte("half a req"))
			require.NoError(t, err)
		case 2: // request finished, response pending
			_, err := conns[i].Write([]byte("whole request"))
			require.NoError(t, err)
			require.NoError(t, conns[i].CloseWrite())
		case 3: // exchanged data and still open
			_, err := conns[i].Write([]byte("ping"))
			require.NoError(t, err)
			buf := make([]byte, 4)
			_, err = io.ReadFull(conns[i], buf)
			require.NoError(t, err)
		}
	}
	// Connections that already finished (case 2) may have gone; all the
	// others are open on both sides now.
	require.Eventually(t, func() bool { return guestOpen.Load() >= clients*3/4 }, wait, time.Millisecond)

	closeWithin(t, f)
	assert.Zero(t, tracked(f))
	for i, c := range conns {
		_, err := io.Copy(io.Discard, c)
		var ne net.Error
		require.False(t, errors.As(err, &ne) && ne.Timeout(), "client %d was left open after Close", i)
	}
	// Every guest-side connection was closed too, and the port is free for
	// the next VM that publishes it.
	require.Eventually(t, func() bool { return guestOpen.Load() == 0 }, wait, 5*time.Millisecond)
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err, "the published port stayed bound after Close")
	require.NoError(t, ln.Close())

	// Closing again, from several callers, is harmless.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); f.Close() }()
	}
	wg.Wait()
	var none *Forwarder
	none.Close()
}

func TestForwarderFaults_ManyConcurrentConnections(t *testing.T) {
	b := startBackend(t, func(c *net.TCPConn) {
		_, _ = io.Copy(c, c)
		_ = c.CloseWrite()
	})
	f, addr := publish(t, b.port())

	const clients = 120
	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Sizes from one byte to beyond a socket buffer, each stream
			// distinct, so mixed-up or truncated connections show.
			payload := bytes.Repeat([]byte{byte(i), byte(i >> 8), 0xa5}, 1+i*700)
			c, err := net.DialTimeout("tcp", addr, wait)
			if !assert.NoError(t, err) {
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(3 * wait))
			go func() {
				_, _ = c.Write(payload)
				_ = c.(*net.TCPConn).CloseWrite()
			}()
			got, err := io.ReadAll(c)
			if assert.NoError(t, err, "client %d", i) {
				assert.True(t, bytes.Equal(payload, got), "client %d: got %d of %d bytes", i, len(got), len(payload))
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, clients, b.accepted.Load())
	require.Eventually(t, func() bool { return tracked(f) == 0 }, wait, 5*time.Millisecond, "finished connections stayed tracked")
}

func TestForwarderFaults_PortAlreadyInUse(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer busy.Close()
	busyPort := uint16(busy.Addr().(*net.TCPAddr).Port)

	free1, free2 := freeTCPPort(t), freeTCPPort(t)
	_, err = StartForwarder("127.0.0.1", []PortForward{
		{HostPort: free1, GuestPort: 80, Protocol: "tcp", BindAddr: "127.0.0.1"},
		{HostPort: busyPort, GuestPort: 81, Protocol: "tcp", BindAddr: "127.0.0.1"},
		{HostPort: free2, GuestPort: 82, Protocol: "tcp", BindAddr: "127.0.0.1"},
	})
	require.ErrorContains(t, err, fmt.Sprintf("listen on 127.0.0.1:%d", busyPort))

	// A VM that failed to start holds none of its ports: the ones opened
	// before the conflict were released.
	for _, p := range []uint16{free1, free2} {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		require.NoError(t, err, "port %d stayed bound after the failed publication", p)
		require.NoError(t, ln.Close())
	}
	// The port that was busy still belongs to whoever had it.
	c, err := net.DialTimeout("tcp", busy.Addr().String(), wait)
	require.NoError(t, err)
	require.NoError(t, c.Close())
}

func TestForwarderFaults_SamePortTwiceInOneRequest(t *testing.T) {
	p := freeTCPPort(t)
	_, err := StartForwarder("127.0.0.1", []PortForward{
		{HostPort: p, GuestPort: 80, Protocol: "tcp", BindAddr: "127.0.0.1"},
		{HostPort: p, GuestPort: 81, Protocol: "tcp", BindAddr: "127.0.0.1"},
	})
	require.Error(t, err)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	require.NoError(t, err)
	require.NoError(t, ln.Close())
}

func TestForwarderFaults_UnusableBindAddress(t *testing.T) {
	for _, bind := range []string{"999.1.1.1", "not an address", "127.0.0.1:80", "192.0.2.1", "[::1", "127.0.0.1 "} {
		_, err := StartForwarder("127.0.0.1", []PortForward{{HostPort: freeTCPPort(t), GuestPort: 80, Protocol: "tcp", BindAddr: bind}})
		require.Error(t, err, "bind address %q", bind)
	}
}

// stubConn stands for an open client connection without using a descriptor.
type stubConn struct {
	net.Conn
	closed atomic.Bool
}

func (s *stubConn) Close() error { s.closed.Store(true); return nil }

func TestForwarderFaults_ConnectionLimit(t *testing.T) {
	b := startBackend(t, func(c *net.TCPConn) { _, _ = io.Copy(c, c) })
	f, addr := publish(t, b.port())

	// The forwarder is at its limit: maxProxyConns clients are connected.
	stubs := make([]*stubConn, maxProxyConns)
	f.mu.Lock()
	f.connections = make(map[net.Conn]struct{}, maxProxyConns)
	for i := range stubs {
		stubs[i] = &stubConn{}
		f.connections[stubs[i]] = struct{}{}
	}
	f.mu.Unlock()

	// One more is turned away without reaching the guest, and without
	// stopping the listener.
	for range 3 {
		requireEnded(t, dial(t, addr))
	}
	assert.Zero(t, b.accepted.Load(), "a connection over the limit reached the guest")
	assert.Equal(t, maxProxyConns, tracked(f))

	// A slot frees up and the next client is served.
	f.mu.Lock()
	delete(f.connections, stubs[0])
	f.mu.Unlock()
	c := dial(t, addr)
	_, err := c.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf))

	// Stopping closes every tracked connection.
	closeWithin(t, f)
	for i, s := range stubs[1:] {
		require.True(t, s.closed.Load(), "connection %d was not closed", i+1)
	}
}

func TestForwarderFaults_StartAndStopUnderConnectingClients(t *testing.T) {
	b := startBackend(t, func(c *net.TCPConn) { _, _ = io.Copy(c, c) })
	// Clients keep connecting while the port is published and unpublished,
	// as when a VM restarts under load. Every round must end; a connection
	// accepted during Close must not be left open or hang the stop.
	for range 30 {
		hostPort := freeTCPPort(t)
		f, err := StartForwarder("127.0.0.1", []PortForward{{HostPort: hostPort, GuestPort: b.port(), Protocol: "tcp", BindAddr: "127.0.0.1"}})
		require.NoError(t, err)
		addr := fmt.Sprintf("127.0.0.1:%d", hostPort)

		stop := make(chan struct{})
		var wg sync.WaitGroup
		var open sync.Map
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					c, err := net.DialTimeout("tcp", addr, wait)
					if err != nil {
						continue
					}
					open.Store(c, true)
					_, _ = c.Write([]byte("x"))
				}
			}()
		}
		require.Eventually(t, func() bool { return tracked(f) > 0 }, wait, time.Millisecond)
		closeWithin(t, f)
		close(stop)
		wg.Wait()
		assert.Zero(t, tracked(f))
		open.Range(func(k, _ any) bool {
			c := k.(net.Conn)
			_ = c.SetReadDeadline(time.Now().Add(wait))
			_, err := io.Copy(io.Discard, c)
			var ne net.Error
			assert.False(t, errors.As(err, &ne) && ne.Timeout(), "a client accepted during Close was left open")
			_ = c.Close()
			return true
		})
	}
}

// brokenNAT fails the step it is told to.
type brokenNAT struct {
	fakeNAT
	failBridge, failRemove bool
}

func (b *brokenNAT) prepareBridge(bridge string) error {
	if b.failBridge {
		return errors.New("route_localnet: read-only file system")
	}
	return b.fakeNAT.prepareBridge(bridge)
}

func (b *brokenNAT) remove(e []natEntry) error {
	_ = b.fakeNAT.remove(e)
	if b.failRemove {
		return errors.New("nft: injected failure")
	}
	return nil
}

func TestForwarderFaults_KernelNATUnavailableKeepsTheProxy(t *testing.T) {
	echo := func(c *net.TCPConn) { _, _ = io.Copy(c, c) }
	for name, tc := range map[string]struct {
		nat   *brokenNAT
		guest string
	}{
		"bridge cannot be prepared": {&brokenNAT{failBridge: true}, "127.0.0.1"},
		"rules cannot be added":     {&brokenNAT{fakeNAT: fakeNAT{failAdd: true}}, "127.0.0.1"},
		"guest address is not IPv4": {&brokenNAT{}, "::1"},
	} {
		t.Run(name, func(t *testing.T) {
			ln, err := net.Listen("tcp", net.JoinHostPort(tc.guest, "0"))
			if err != nil {
				t.Skipf("no %s loopback on this machine: %v", tc.guest, err)
			}
			b := serveBackend(t, ln, echo)
			withNAT(t, tc.nat)
			hostPort := freeTCPPort(t)
			f, err := PublishPorts(tc.guest, "jerboa-br0", []PortForward{{HostPort: hostPort, GuestPort: b.port(), Protocol: "tcp", BindAddr: "127.0.0.1"}})
			require.NoError(t, err, "a kernel NAT failure must not fail the VM")
			defer f.Close()
			assert.Empty(t, tc.nat.added)

			// The userspace proxy still serves the port.
			c := dial(t, fmt.Sprintf("127.0.0.1:%d", hostPort))
			_, err = c.Write([]byte("ping"))
			require.NoError(t, err)
			buf := make([]byte, 4)
			_, err = io.ReadFull(c, buf)
			require.NoError(t, err)
			assert.Equal(t, "ping", string(buf))

			// Nothing was installed, so nothing is removed.
			closeWithin(t, f)
			assert.Empty(t, tc.nat.removed)
		})
	}
}

func TestForwarderFaults_KernelNATRemovedOnceEvenWhenRemovalFails(t *testing.T) {
	n := &brokenNAT{failRemove: true}
	withNAT(t, n)
	tcp, udp := freePort(t, "tcp"), freePort(t, "udp")
	f, err := PublishPorts("10.100.0.9", "jerboa-br0", []PortForward{
		{HostPort: tcp, GuestPort: 80, Protocol: "tcp", BindAddr: "127.0.0.1"},
		{HostPort: udp, GuestPort: 53, Protocol: "udp", BindAddr: "127.0.0.1"},
	})
	require.NoError(t, err)
	require.Len(t, n.added, 2)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); f.Close() }()
	}
	wg.Wait()
	// The failed removal is not retried by the other callers, and the ports
	// are released regardless, so the next VM can publish them.
	assert.Len(t, n.removed, 2)
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(tcp)))
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", itoa(udp)))
	require.NoError(t, err)
	require.NoError(t, pc.Close())
}

func TestForwarderFaults_UDPPortInUseReleasesEverything(t *testing.T) {
	n := &brokenNAT{}
	withNAT(t, n)
	busy, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer busy.Close()
	busyPort := uint16(busy.LocalAddr().(*net.UDPAddr).Port)
	tcp, udp := freePort(t, "tcp"), freePort(t, "udp")

	_, err = PublishPorts("10.100.0.9", "jerboa-br0", []PortForward{
		{HostPort: tcp, GuestPort: 80, Protocol: "tcp", BindAddr: "127.0.0.1"},
		{HostPort: udp, GuestPort: 53, Protocol: "udp", BindAddr: "127.0.0.1"},
		{HostPort: busyPort, GuestPort: 54, Protocol: "UDP", BindAddr: "127.0.0.1"},
	})
	require.ErrorContains(t, err, "listen on udp")
	// No translation points at a VM that never started, and its ports are free.
	assert.Empty(t, n.added)
	assert.Empty(t, n.bridges)
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(tcp)))
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", itoa(udp)))
	require.NoError(t, err)
	require.NoError(t, pc.Close())
}

func TestNATEntries_HostileInput(t *testing.T) {
	ports := []PortForward{{HostPort: 80, GuestPort: 8080, Protocol: "tcp"}}
	for _, guest := range []string{"", "garbage", "::1", "fe80::1%eth0", "10.0.0", "10.0.0.256", "10.0.0.1/24", " 10.0.0.1", "10.0.0.1\n"} {
		_, err := natEntries(guest, ports)
		require.Error(t, err, "guest %q", guest)
	}

	// A bind address the kernel map cannot hold stays on the proxy only; it
	// must never become a rule for every address ("any") by accident.
	for _, bind := range []string{"::1", "fe80::1", "localhost", "garbage", "127.0.0.1:80", "127.0.0.1/8", " 127.0.0.1", "0.0.0.0.0"} {
		got, err := natEntries("10.0.0.2", []PortForward{{HostPort: 80, GuestPort: 8080, Protocol: "tcp", BindAddr: bind}})
		require.NoError(t, err)
		require.Empty(t, got, "bind %q became a kernel translation", bind)
	}
	// An unknown protocol is published as TCP, like the listeners do.
	got, err := natEntries("::ffff:10.0.0.2", []PortForward{{HostPort: 1, GuestPort: 2, Protocol: "sctp; drop"}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "tcp", got[0].proto)
	assert.Equal(t, "10.0.0.2", got[0].guest.String())
	assert.Len(t, got[0].guest, net.IPv4len)
}

func FuzzNATEntries(f *testing.F) {
	f.Add("10.100.0.9", "127.0.0.1", "tcp", uint16(8080), uint16(80))
	f.Add("10.100.0.9", "", "udp", uint16(53), uint16(53))
	f.Add("10.100.0.9", "::", "UDP", uint16(0), uint16(0))
	f.Add("::1", "0.0.0.0", "", uint16(65535), uint16(1))
	f.Add("", "::1", "tcp", uint16(1), uint16(1))
	f.Add("::ffff:10.0.0.2", "::ffff:127.0.0.1", "Udp", uint16(1), uint16(1))
	f.Add("10.0.0.2 }\nflush ruleset", "127.0.0.1 }\nflush ruleset", "tcp }", uint16(1), uint16(1))
	f.Fuzz(func(t *testing.T, guest, bind, proto string, hostPort, guestPort uint16) {
		entries, err := natEntries(guest, []PortForward{{HostPort: hostPort, GuestPort: guestPort, Protocol: proto, BindAddr: bind}})
		if err != nil {
			if len(entries) != 0 {
				t.Fatalf("entries returned with an error: %v", entries)
			}
			return
		}
		// Entries are rendered into a firewall script: every field must be
		// a parsed value, never text taken from the request.
		for _, e := range entries {
			if len(e.guest) != net.IPv4len {
				t.Fatalf("guest %q became %v", guest, e.guest)
			}
			if e.bind != nil && len(e.bind) != net.IPv4len {
				t.Fatalf("bind %q became %v", bind, e.bind)
			}
			if e.proto != "tcp" && e.proto != "udp" {
				t.Fatalf("protocol %q became %q", proto, e.proto)
			}
			if e.hostPort != hostPort || e.guestPort != guestPort {
				t.Fatalf("ports changed: %d->%d became %d->%d", hostPort, guestPort, e.hostPort, e.guestPort)
			}
		}
	})
}
