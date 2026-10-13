package network

import (
	"encoding/binary"
	"errors"
	"strings"
	"sync/atomic"
)

// Peer control protocol shared with the macOS VMM (firecracker-macos
// src/hvf-vmm/native/peer_proto.h). The switch stays the control plane: it
// hands two VMMs on one network the ends of a SOCK_DGRAM socketpair, and
// unicast frames between them then skip the switch. Every message is 32 wire
// bytes, little endian:
//
//	0  "JPC1"        8  u64 epoch (STATS: TX bytes)
//	4  u8 type       16 u32 peer id / HELLO features (STATS: u64 RX bytes)
//	5  u8 flags      20 MAC[6]   26 IPv4[4]   6, 30: u16 reserved, zero
const peerMessageSize = 32

const (
	peerHello   = 1 // VMM: features, own MAC
	peerWelcome = 2 // switch: epoch, own MAC and IP
	peerAdd     = 3 // switch: epoch, id, peer MAC and IP, one SCM_RIGHTS descriptor
	peerDel     = 4 // switch: epoch, id
	peerAck     = 5 // VMM: epoch, id, flags=1 when accepted
	peerDead    = 6 // VMM: epoch, id; the link failed and was removed
	peerReset   = 7 // broker to VMM only
	peerStats   = 8 // VMM: cumulative direct TX and RX bytes

	peerFeatureDgram = 1
)

type peerMessage struct {
	Type, Flags uint8
	Epoch       uint64 // STATS: TX bytes
	Value       uint64 // peer id or features; STATS: RX bytes
	MAC         [6]byte
	IP          [4]byte
}

func (m peerMessage) encode() [peerMessageSize]byte {
	var b [peerMessageSize]byte
	copy(b[:4], "JPC1")
	b[4], b[5] = m.Type, m.Flags
	binary.LittleEndian.PutUint64(b[8:16], m.Epoch)
	if m.Type == peerStats {
		binary.LittleEndian.PutUint64(b[16:24], m.Value)
		return b
	}
	binary.LittleEndian.PutUint32(b[16:20], uint32(m.Value))
	copy(b[20:26], m.MAC[:])
	copy(b[26:30], m.IP[:])
	return b
}

var errPeerMessage = errors.New("invalid peer control message")

func decodePeerMessage(b []byte) (peerMessage, error) {
	var m peerMessage
	if len(b) != peerMessageSize || string(b[:4]) != "JPC1" || b[6] != 0 || b[7] != 0 || b[4] < peerHello || b[4] > peerStats {
		return m, errPeerMessage
	}
	m.Type, m.Flags = b[4], b[5]
	m.Epoch = binary.LittleEndian.Uint64(b[8:16])
	if m.Type == peerStats {
		m.Value = binary.LittleEndian.Uint64(b[16:24])
		return m, nil
	}
	if b[30] != 0 || b[31] != 0 {
		return m, errPeerMessage
	}
	m.Value = uint64(binary.LittleEndian.Uint32(b[16:20]))
	copy(m.MAC[:], b[20:26])
	copy(m.IP[:], b[26:30])
	return m, nil
}

var nativePeering atomic.Bool

func init() { nativePeering.Store(true) }

// SetNativePeering enables or disables direct VM-to-VM links for macOS
// networks created afterwards (jerboad --native-peering).
func SetNativePeering(enabled bool) { nativePeering.Store(enabled) }

// NativePeeringEnabled reports whether new macOS networks offer direct links.
func NativePeeringEnabled() bool { return nativePeering.Load() }

// PeerControlPath is the control socket that accompanies a VM's Ethernet
// stream socket, in the same private directory.
func PeerControlPath(stream string) string {
	return strings.TrimSuffix(stream, ".sock") + ".ctl"
}
