//go:build !linux

package apiserver

import "net"

func loopbackOwner(_, _ *net.TCPAddr) (uint32, bool) { return 0, false }
