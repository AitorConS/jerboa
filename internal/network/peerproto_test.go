package network

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The byte layout must match the VMM's peer_proto.h (test-net-peer.c checks
// the same offsets from the C side).
func TestPeerMessageWireFormat(t *testing.T) {
	m := peerMessage{Type: peerAdd, Flags: 1, Epoch: 0x1122334455667788, Value: 0xa0b0c0d0, MAC: [6]byte{2, 0, 0, 0, 0, 2}, IP: [4]byte{10, 0, 0, 2}}
	b := m.encode()
	require.Equal(t, "JPC1", string(b[:4]))
	require.Equal(t, []byte{peerAdd, 1, 0, 0, 0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, 0xd0, 0xc0, 0xb0, 0xa0}, b[4:20])
	require.Equal(t, []byte{2, 0, 0, 0, 0, 2, 10, 0, 0, 2, 0, 0}, b[20:32])
	got, err := decodePeerMessage(b[:])
	require.NoError(t, err)
	require.Equal(t, m, got)

	stats := peerMessage{Type: peerStats, Epoch: 1 << 40, Value: 1<<40 + 5}
	b = stats.encode()
	got, err = decodePeerMessage(b[:])
	require.NoError(t, err)
	require.Equal(t, stats, got)
}

func TestPeerMessageRejectsMalformed(t *testing.T) {
	good := peerMessage{Type: peerHello, Value: peerFeatureDgram}.encode()
	for name, mutate := range map[string]func(b []byte){
		"magic":     func(b []byte) { b[0] = 'X' },
		"type zero": func(b []byte) { b[4] = 0 },
		"type high": func(b []byte) { b[4] = 10 },
		"reserved":  func(b []byte) { b[6] = 1 },
		"tail":      func(b []byte) { b[31] = 1 },
	} {
		b := good
		mutate(b[:])
		_, err := decodePeerMessage(b[:])
		require.Error(t, err, name)
	}
	_, err := decodePeerMessage(good[:31])
	require.Error(t, err)
}
