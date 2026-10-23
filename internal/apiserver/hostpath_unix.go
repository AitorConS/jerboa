//go:build linux || darwin

package apiserver

import (
	"io/fs"
	"net"
	"syscall"
)

func fileOwner(info fs.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func peerUID(conn *net.UnixConn) (uint32, bool) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var uid uint32
	var ok bool
	if err := raw.Control(func(fd uintptr) { uid, ok = socketPeerUID(int(fd)) }); err != nil { //nolint:gosec // fds fit in int
		return 0, false
	}
	return uid, ok
}
