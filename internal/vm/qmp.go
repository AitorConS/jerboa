//go:build linux

package vm

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// qmpDo connects to a QEMU Machine Protocol socket at addr, negotiates
// capabilities, and executes the given QMP command. addr is "unix:<path>"
// (default on Linux) or "tcp:host:port" for backwards compatibility.
func qmpDo(addr, command string) error {
	network, address := parseQMPAddr(addr)
	conn, err := net.DialTimeout(network, address, 3*time.Second) //nolint:noctx // QMP dial has a built-in 3s timeout; no caller context available
	if err != nil {
		return fmt.Errorf("qmp dial %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	dec := json.NewDecoder(conn)

	// Read the QMP greeting: {"QMP": {"version": ..., "capabilities": []}}.
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("qmp greeting: %w", err)
	}

	// Negotiate capabilities before issuing commands (required by protocol).
	if _, err := fmt.Fprintf(conn, `{"execute":"qmp_capabilities"}`+"\n"); err != nil {
		return fmt.Errorf("qmp capabilities: %w", err)
	}
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("qmp capabilities ack: %w", err)
	}

	// Send the actual command.
	if _, err := fmt.Fprintf(conn, `{"execute":%q}`+"\n", command); err != nil {
		return fmt.Errorf("qmp execute %s: %w", command, err)
	}

	// Read response, skipping async event messages QEMU may send before the reply.
	for {
		var msg map[string]json.RawMessage
		if err := dec.Decode(&msg); err != nil {
			return fmt.Errorf("qmp response: %w", err)
		}
		if _, isEvent := msg["event"]; isEvent {
			continue
		}
		if errMsg, hasErr := msg["error"]; hasErr {
			var qmpErr struct{ Class, Desc string }
			_ = json.Unmarshal(errMsg, &qmpErr)
			return fmt.Errorf("qmp %s: %s: %s", command, qmpErr.Class, qmpErr.Desc)
		}
		return nil
	}
}

// parseQMPAddr splits a QMP address into a net dial network and address.
// "unix:<path>" → ("unix", "<path>"); "tcp:host:port" or bare "host:port" → tcp.
func parseQMPAddr(addr string) (network, address string) {
	switch {
	case strings.HasPrefix(addr, "unix:"):
		return "unix", strings.TrimPrefix(addr, "unix:")
	case strings.HasPrefix(addr, "tcp:"):
		return "tcp", strings.TrimPrefix(addr, "tcp:")
	default:
		return "tcp", addr
	}
}

// qmpSocketPath returns the Unix domain socket path used for a VM's QMP channel.
// A Unix socket is bound directly by QEMU, so there is no ephemeral-port race
// window (unlike a TCP listener probed and closed before QEMU starts).
func qmpSocketPath(id string) string {
	return filepath.Join(os.TempDir(), "jerboa-qmp-"+id+".sock")
}

// removeQMPSocket deletes the Unix socket file backing a QMP address, if any.
func removeQMPSocket(addr string) {
	if network, path := parseQMPAddr(addr); network == "unix" {
		_ = os.Remove(path)
	}
}
