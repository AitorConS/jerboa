package network

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The peer control socket is written by a VMM, a separate process that can
// crash mid-message or be a different version. Whatever arrives, decoding
// either yields a message the switch understands or an error; it never
// panics and never reads a short buffer as a message.

func TestPeerMessage_TruncatedAndOversized(t *testing.T) {
	for _, m := range []peerMessage{
		{Type: peerHello, Value: peerFeatureDgram, MAC: [6]byte{2, 0, 0, 0, 0, 1}},
		{Type: peerAdd, Flags: 1, Epoch: 9, Value: 7, MAC: [6]byte{2, 0, 0, 0, 0, 2}, IP: [4]byte{10, 0, 0, 2}},
		{Type: peerStats, Epoch: 1 << 50, Value: 1 << 51},
	} {
		wire := m.encode()
		// A VMM that died while writing: every prefix of a valid message.
		for n := range len(wire) {
			_, err := decodePeerMessage(wire[:n])
			require.ErrorIs(t, err, errPeerMessage, "type %d cut to %d bytes", m.Type, n)
		}
		// Two messages read as one, or trailing bytes.
		for _, extra := range []int{1, 2, peerMessageSize} {
			_, err := decodePeerMessage(append(wire[:], make([]byte, extra)...))
			require.ErrorIs(t, err, errPeerMessage, "type %d with %d extra bytes", m.Type, extra)
		}
	}
	_, err := decodePeerMessage(nil)
	require.ErrorIs(t, err, errPeerMessage)
}

func TestPeerMessage_EveryTypeAndReservedByte(t *testing.T) {
	base := peerMessage{Type: peerAck, Flags: 1, Epoch: 3, Value: 4}.encode()
	for typ := range 256 {
		b := base
		b[4] = byte(typ)
		m, err := decodePeerMessage(b[:])
		if typ >= peerHello && typ <= peerIdle {
			require.NoError(t, err, "type %d", typ)
			require.Equal(t, uint8(typ), m.Type)
			continue
		}
		require.ErrorIs(t, err, errPeerMessage, "type %d is not part of the protocol", typ)
	}
	// Reserved bytes are zero today; a message that sets one comes from a
	// protocol this switch does not speak and must not be half-understood.
	for _, at := range []int{6, 7, 30, 31} {
		for _, v := range []byte{1, 0x80, 0xff} {
			b := base
			b[at] = v
			_, err := decodePeerMessage(b[:])
			require.ErrorIs(t, err, errPeerMessage, "reserved byte %d = %#x", at, v)
		}
	}
	// Each byte of the magic matters, in either case.
	for at := range 4 {
		b := base
		b[at] ^= 0x20
		_, err := decodePeerMessage(b[:])
		require.ErrorIs(t, err, errPeerMessage, "magic byte %d", at)
	}
}

func TestPeerMessage_IDsWiderThanTheWireAreNotSilentlyAliased(t *testing.T) {
	// Pair ids travel as 32 bits. Encoding keeps the low half, so the switch
	// must never issue an id above that: this pins what would collide.
	wide := peerMessage{Type: peerDel, Epoch: 1, Value: 1<<32 + 5}.encode()
	narrow := peerMessage{Type: peerDel, Epoch: 1, Value: 5}.encode()
	assert.Equal(t, narrow, wide)
	// Statistics use the full 64 bits.
	a := peerMessage{Type: peerStats, Epoch: 1, Value: 1<<32 + 5}.encode()
	b := peerMessage{Type: peerStats, Epoch: 1, Value: 5}.encode()
	assert.NotEqual(t, a, b)
	got, err := decodePeerMessage(a[:])
	require.NoError(t, err)
	assert.Equal(t, uint64(1<<32+5), got.Value)
}

func TestPeerControlPath(t *testing.T) {
	for stream, want := range map[string]string{
		"/run/jerboa/vm1.sock":      "/run/jerboa/vm1.ctl",
		"/run/jerboa/vm1":           "/run/jerboa/vm1.ctl",
		"/run/my.sock.dir/vm1.sock": "/run/my.sock.dir/vm1.ctl",
		"vm.sock.sock":              "vm.sock.ctl",
		".sock":                     ".ctl",
		"":                          ".ctl",
	} {
		got := PeerControlPath(stream)
		assert.Equal(t, want, got)
		// The control socket must never be the stream socket itself:
		// listening on it would replace the VM's Ethernet link.
		assert.NotEqual(t, stream, got)
	}
}

func FuzzDecodePeerMessage(f *testing.F) {
	for _, m := range []peerMessage{
		{Type: peerHello, Value: peerFeatureDgram | 2, MAC: [6]byte{2, 0, 0, 0, 0, 1}},
		{Type: peerWelcome, Epoch: 1, MAC: [6]byte{2, 0, 0, 0, 0, 1}, IP: [4]byte{10, 0, 0, 1}},
		{Type: peerAdd, Flags: 1, Epoch: 1 << 63, Value: 1<<32 - 1},
		{Type: peerStats, Epoch: ^uint64(0), Value: ^uint64(0)},
		{Type: peerIdle, Epoch: 2, Value: 3},
	} {
		b := m.encode()
		f.Add(b[:])
		f.Add(b[:peerMessageSize-1])
	}
	f.Add([]byte{})
	f.Add([]byte("JPC1"))
	f.Add(bytes.Repeat([]byte{0xff}, peerMessageSize))
	f.Add(bytes.Repeat([]byte("JPC1"), 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := decodePeerMessage(b)
		if err != nil {
			if m != (peerMessage{}) {
				t.Fatalf("a rejected message leaked fields: %+v", m)
			}
			return
		}
		if len(b) != peerMessageSize {
			t.Fatalf("%d bytes were accepted as a message", len(b))
		}
		if m.Type < peerHello || m.Type > peerIdle {
			t.Fatalf("type %d accepted", m.Type)
		}
		// What was understood is all there is to the message: encoding it
		// again and decoding gives the same message, so nothing the switch
		// acts on depends on bytes it ignored.
		wire := m.encode()
		again, err := decodePeerMessage(wire[:])
		if err != nil || again != m {
			t.Fatalf("%+v does not survive a round trip: %+v, %v", m, again, err)
		}
		if m.Type != peerStats && !bytes.Equal(wire[:], b) {
			t.Fatalf("accepted bytes differ from their canonical form:\n%x\n%x", b, wire)
		}
	})
}
