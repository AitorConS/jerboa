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

// loopbackOwner finds the user owning the client end of a loopback TCP
// connection: the socket in /proc/net/tcp{,6} whose local address is the
// connection's remote address and whose peer is the daemon's end.
func loopbackOwner(local, remote *net.TCPAddr) (uint32, bool) {
	file := "/proc/net/tcp"
	if remote.IP.To4() == nil {
		file = "/proc/net/tcp6"
	}
	f, err := os.Open(file)
	if err != nil {
		return 0, false
	}
	defer f.Close() //nolint:errcheck // read-only
	want, peer := procAddr(remote), procAddr(local)
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

// procAddr formats addr the way /proc/net/tcp does: the address as host-order
// 32-bit words in hex, then the port.
func procAddr(addr *net.TCPAddr) string {
	ip := addr.IP.To4()
	if ip == nil {
		ip = addr.IP.To16()
	}
	words := make([]byte, len(ip))
	for i := 0; i < len(ip); i += 4 {
		binary.NativeEndian.PutUint32(words[i:], binary.BigEndian.Uint32(ip[i:]))
	}
	return strings.ToUpper(hex.EncodeToString(words)) + ":" + fmt.Sprintf("%04X", addr.Port)
}
