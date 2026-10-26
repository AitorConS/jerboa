package apiserver

import "net"

// handshakePeer names who is behind an unauthenticated connection, so the
// handshake slots can be shared out per peer: the owning user for a Unix
// socket, or for loopback TCP where the platform can tell, else the remote
// address. One local user or remote host then cannot hold every slot. It is
// empty when the peer cannot be told apart from others (loopback TCP where
// the owner is unknown), and only the overall limit applies.
func handshakePeer(conn net.Conn) string {
	switch c := conn.(type) {
	case *net.UnixConn:
		if uid, ok := peerUID(c); ok {
			return "uid:" + itoa(uid)
		}
		return ""
	case *net.TCPConn:
		remote, _ := c.RemoteAddr().(*net.TCPAddr)
		local, _ := c.LocalAddr().(*net.TCPAddr)
		switch {
		case remote == nil:
			return ""
		case remote.IP.IsLoopback():
			if local == nil {
				return ""
			}
			if uid, ok := loopbackOwner(local, remote); ok {
				return "uid:" + itoa(uid)
			}
			return ""
		}
		return "ip:" + remote.IP.String()
	}
	return ""
}

func itoa(n uint32) string {
	var b [10]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}
