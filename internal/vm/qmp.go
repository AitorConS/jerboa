//go:build linux || (darwin && arm64)

package vm

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// qmpRuntimeDir returns a per-user, 0700 directory for VMM sockets, configs
// and logs, creating it on demand. Keeping them in a private directory rather
// than directly under a world-writable temp dir stops another local process
// from name-squatting a path between cleanup and the VMM's bind. A directory
// at the expected path that is not ours (another owner, a symlink, open
// permissions) is never used: the daemon falls back to a fresh private one.
func qmpRuntimeDir() string {
	runtimeDir.Do(func() {
		dir := filepath.Join(os.TempDir(), fmt.Sprintf("jerboa-run-%d", os.Getuid()))
		_ = os.Mkdir(dir, 0o700)
		err := checkPrivateDir(dir)
		if err == nil {
			runtimeDir.path = dir
			return
		}
		slog.Warn("vm runtime dir is not private, using a new one", "dir", dir, "err", err)
		dir, err = os.MkdirTemp("", "jerboa-run-")
		if err != nil {
			dir = filepath.Join(os.TempDir(), fmt.Sprintf("jerboa-run-%d", os.Getuid()))
		}
		runtimeDir.path = dir
	})
	return runtimeDir.path
}

var runtimeDir struct {
	sync.Once
	path string
}

// checkPrivateDir reports an error unless dir is a real directory owned by
// this user and closed to others.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat runtime dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by uid %d", dir, st.Uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("restrict runtime dir: %w", err)
		}
	}
	return nil
}

// qmpSocketPath returns the Unix domain socket path used for a VM's QMP channel.
// A Unix socket is bound directly by QEMU, so there is no ephemeral-port race
// window (unlike a TCP listener probed and closed before QEMU starts).
func qmpSocketPath(id string) string {
	return filepath.Join(qmpRuntimeDir(), "qmp-"+id+".sock")
}

// removeQMPSocket deletes the Unix socket file backing a QMP address, if any.
func removeQMPSocket(addr string) {
	if network, path := parseQMPAddr(addr); network == "unix" {
		_ = os.Remove(path)
	}
}
