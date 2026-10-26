package apiserver

import "golang.org/x/sys/unix"

func socketPeerUID(fd int) (uint32, bool) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, false
	}
	return cred.Uid, true
}
