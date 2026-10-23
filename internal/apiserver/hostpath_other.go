//go:build !linux && !darwin

package apiserver

import (
	"io/fs"
	"net"
)

func fileOwner(fs.FileInfo) (uint32, bool) { return 0, false }

func peerUID(*net.UnixConn) (uint32, bool) { return 0, false }
