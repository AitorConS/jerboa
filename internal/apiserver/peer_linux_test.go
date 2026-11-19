package apiserver

import (
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHandshakePeer_LoopbackOwner(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			addr := "127.0.0.1:0"
			if network == "tcp6" {
				addr = "[::1]:0"
			}
			ln, err := net.Listen(network, addr)
			if err != nil {
				t.Skip("no " + network + " loopback")
			}
			defer ln.Close()
			client, err := net.Dial(network, ln.Addr().String())
			require.NoError(t, err)
			defer client.Close()
			server, err := ln.Accept()
			require.NoError(t, err)
			defer server.Close()
			require.Equal(t, "uid:"+itoa(uint32(os.Getuid())), handshakePeer(server)) //nolint:gosec // uids fit in uint32
		})
	}
}

// TestHandshakePeer_MappedLoopbackOwner covers a client on an AF_INET6 socket
// that dials the IPv4 loopback listener through ::ffff:127.0.0.1: the daemon
// sees 127.0.0.1, but the client socket is listed only in /proc/net/tcp6.
func TestHandshakePeer_MappedLoopbackOwner(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	// net.Dial will not put an IPv4-mapped destination on an AF_INET6
	// socket, so build the client by hand.
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skip("no IPv6 sockets: ", err)
	}
	defer syscall.Close(fd) //nolint:errcheck // test cleanup
	require.NoError(t, syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0))
	sa := &syscall.SockaddrInet6{Port: port}
	copy(sa.Addr[:], net.ParseIP("::ffff:127.0.0.1").To16())
	require.NoError(t, syscall.Connect(fd, sa))
	server, err := ln.Accept()
	require.NoError(t, err)
	defer server.Close()
	require.Equal(t, "uid:"+itoa(uint32(os.Getuid())), handshakePeer(server)) //nolint:gosec // uids fit in uint32
}

func TestHandshakePeer_UnattributedLoopbackIsCapped(t *testing.T) {
	// A loopback address pair with no matching socket in /proc lands in the
	// shared bucket, not the uncapped empty key.
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}
	_, ok := loopbackOwner(local, remote)
	require.False(t, ok)
	require.NotEmpty(t, unattributedPeer)

	srv := &Server{handshakes: make(chan struct{}, maxPendingHandshake)}
	for range maxPendingPerPeer + 1 {
		h, _ := claim(t, srv, unattributedPeer)
		require.NotNil(t, h)
	}
	require.Len(t, srv.pending[unattributedPeer], maxPendingPerPeer, "unattributed peers share one capped bucket")
	require.Len(t, srv.handshakes, maxPendingPerPeer)
	h, _ := claim(t, srv, "uid:1000")
	require.NotNil(t, h, "attributed peers still get a slot")
}
