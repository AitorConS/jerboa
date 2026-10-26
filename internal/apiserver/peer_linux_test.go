package apiserver

import (
	"net"
	"os"
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
