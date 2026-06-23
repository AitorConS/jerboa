package api

import (
	"fmt"
	"strings"
)

// parseEndpoint splits a daemon endpoint into a net network and address.
//
// Supported schemes:
//
//	unix:///var/run/unid.sock  -> "unix", "/var/run/unid.sock"
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
