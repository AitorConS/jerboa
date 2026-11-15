//go:build !linux

package apiserver

import "net"

// unattributedPeer is empty where loopback owners cannot be looked up at all:
// every loopback client would share one bucket, and an operator running more
// than maxPendingPerPeer commands at once would be refused, so only the
// overall limit applies.
const unattributedPeer = ""

func loopbackOwner(_, _ *net.TCPAddr) (uint32, bool) { return 0, false }
