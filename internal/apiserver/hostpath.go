package apiserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

type peerKey struct{}

// withPeer records the connection a request came from, so handlers can tell
// a local caller's user from a remote one.
func withPeer(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, peerKey{}, conn)
}

// callerUID returns the user id of a local caller on a Unix socket.
func callerUID(ctx context.Context) (uint32, bool) {
	conn, ok := ctx.Value(peerKey{}).(*net.UnixConn)
	if !ok {
		return 0, false
	}
	return peerUID(conn)
}

// hostImagePathCheck is checkHostImagePath; tests that run fake image files
// replace it.
var hostImagePathCheck = checkHostImagePath

// checkHostImagePath allows a client-supplied disk path only when a local
// caller owns the file (or is root, or is the daemon's own user): the daemon
// boots it with its own privileges, so it must not read files the caller
// could not.
func checkHostImagePath(ctx context.Context, p string) (string, error) {
	uid, ok := callerUID(ctx)
	if !ok {
		return "", fmt.Errorf("image paths are accepted only from local clients; build or pull the image instead")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("image path %q must be absolute", p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("image path: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("image path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("image path %q is not a regular file", p)
	}
	if uid == 0 || uid == uint32(os.Getuid()) { //nolint:gosec // uids fit in 32 bits
		return real, nil
	}
	if owner, ok := fileOwner(info); !ok || owner != uid {
		return "", fmt.Errorf("image path %q is not owned by the calling user", p)
	}
	return real, nil
}
