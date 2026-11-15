package api

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// parseEndpoint splits a daemon endpoint into a net network and address.
//
// Supported schemes:
//
//	unix:///var/run/jerboad.sock  -> "unix", "/var/run/jerboad.sock"
//	tcp://127.0.0.1:7890       -> "tcp",  "127.0.0.1:7890"
//
// A value without a "://" scheme is treated as a Unix socket path, preserving
// backward compatibility with the legacy bare --socket flag.
func parseEndpoint(endpoint string) (network, address string, err error) {
	scheme, rest, found := strings.Cut(endpoint, "://")
	if !found {
		if endpoint == "" {
			return "", "", fmt.Errorf("api: empty endpoint")
		}
		return "unix", endpoint, nil
	}
	switch scheme {
	case "unix":
		if rest == "" {
			return "", "", fmt.Errorf("api: empty unix socket path in endpoint %q", endpoint)
		}
		return "unix", rest, nil
	case "tcp":
		if rest == "" {
			return "", "", fmt.Errorf("api: empty tcp address in endpoint %q", endpoint)
		}
		return "tcp", rest, nil
	default:
		return "", "", fmt.Errorf("api: unsupported endpoint scheme %q (use unix:// or tcp://)", scheme)
	}
}

// Listen binds a net.Listener for the given endpoint, clearing a stale Unix
// socket file first. Used by the server to accept connections.
func Listen(endpoint string) (net.Listener, error) {
	network, address, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if err := clearStaleSocket(address); err != nil {
			return nil, err
		}
	}
	restoreUmask := func() {}
	if network == "unix" {
		// syscall.Umask is process-global; this narrowly scopes it around the
		// startup bind so the socket is never created with wider permissions.
		restoreUmask = restrictUnixSocketUmask()
	}
	l, err := net.Listen(network, address) //nolint:noctx // server setup; lifecycle managed by Serve's ctx
	restoreUmask()
	if err != nil {
		return nil, fmt.Errorf("api server listen %s: %w", endpoint, err)
	}
	if network == "unix" {
		// Restrict the socket to the owning user: the daemon is unauthenticated
		// over Unix sockets, so filesystem permissions are the access control.
		// Chmod follows symlinks, so first make sure the path is still the
		// socket just bound and not a link swapped in since.
		if fi, err := os.Lstat(address); err != nil || fi.Mode()&os.ModeSocket == 0 {
			_ = l.Close()
			return nil, fmt.Errorf("api server: %s is not the socket just created", address)
		}
		if err := os.Chmod(address, 0o600); err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("api server chmod socket %s: %w", address, err)
		}
	}
	return l, nil
}

// staleProbe bounds the check for a daemon behind an existing socket file.
const staleProbe = 2 * time.Second

// clearStaleSocket removes the socket file a daemon that is gone left
// behind. It refuses to remove a socket something still answers on (a
// second daemon would start, and the first keep running unreachable with
// its VMs) and anything that is not a socket (a mistyped --socket must not
// delete a file).
func clearStaleSocket(address string) error {
	fi, err := os.Lstat(address)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("api server check socket %s: %w", address, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("api server: %s exists and is not a socket; refusing to replace it", address)
	}
	d := net.Dialer{Timeout: staleProbe}
	if conn, dialErr := d.Dial("unix", address); dialErr == nil { //nolint:noctx // bounded by the dialer's timeout
		_ = conn.Close()
		return fmt.Errorf("api server: a daemon is already listening on %s", address)
	}
	if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("api server remove stale socket: %w", err)
	}
	return nil
}

// dialArgs splits an already-validated endpoint into net.Dial arguments. Used
// for opening secondary connections from a live Client, where the endpoint was
// validated at Dial time; a parse failure falls back to a Unix socket path.
func dialArgs(endpoint string) (network, address string) {
	n, a, err := parseEndpoint(endpoint)
	if err != nil {
		return "unix", endpoint
	}
	return n, a
}
