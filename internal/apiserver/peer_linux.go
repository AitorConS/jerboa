package apiserver

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// unattributedPeer is the shared bucket for peers whose owner cannot be
// found. On Linux that should not happen for a live loopback connection, so
// whatever lands here is capped like any single peer rather than left to take
// every slot.
const unattributedPeer = "unattributed"

// loopbackOwner finds the user owning the client end of a loopback TCP
// connection: the socket in /proc/net/tcp{,6} whose local address is the
// connection's remote address and whose peer is the daemon's end.
func loopbackOwner(local, remote *net.TCPAddr) (uint32, bool) {
	if remote.IP.To4() == nil {
		return procOwner("/proc/net/tcp6", procAddr(remote.IP.To16(), remote.Port), procAddr(local.IP.To16(), local.Port))
	}
	if uid, ok := procOwner("/proc/net/tcp", procAddr(remote.IP.To4(), remote.Port), procAddr(local.IP.To4(), local.Port)); ok {
		return uid, true
	}
	// A client on an AF_INET6 socket that dialed an IPv4-mapped address
	// (::ffff:127.0.0.1) shows up to the daemon as 127.0.0.1 but is listed
	// only in tcp6, under the mapped form of both addresses.
	return procOwner("/proc/net/tcp6", procAddr(remote.IP.To16(), remote.Port), procAddr(local.IP.To16(), local.Port))
}

// procOwner returns the uid of the socket in file (a /proc/net/tcp table)
// whose local and remote addresses are want and peer.
func procOwner(file, want, peer string) (uint32, bool) {
	f, err := os.Open(file)
	if err != nil {
		return 0, false
	}
	defer f.Close() //nolint:errcheck // read-only
	scanner := bufio.NewScanner(f)
	scanner.Scan() // header
	for scanner.Scan() {
		// sl local_address rem_address st tx:rx tr:when retrnsmt uid ...
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[1] != want || fields[2] != peer {
			continue
		}
		uid, err := strconv.ParseUint(fields[7], 10, 32)
		if err != nil {
			return 0, false
		}
		return uint32(uid), true
	}
	return 0, false
}

// procAddr formats ip (4 or 16 bytes) and port the way /proc/net/tcp does:
// the address as host-order 32-bit words in hex, then the port.
func procAddr(ip net.IP, port int) string {
	words := make([]byte, len(ip))
	for i := 0; i < len(ip); i += 4 {
		binary.NativeEndian.PutUint32(words[i:], binary.BigEndian.Uint32(ip[i:]))
	}
	return strings.ToUpper(hex.EncodeToString(words)) + ":" + fmt.Sprintf("%04X", port)
}
