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

// vmRuntimeDir returns a per-user, 0700 directory for VMM sockets, configs
// and logs, creating it on demand. Keeping them in a private directory rather
// than directly under a world-writable temp dir stops another local process
// from name-squatting a path between cleanup and the VMM's bind. A directory
// at the expected path that is not ours (another owner, a symlink, open
// permissions) is never used: the daemon falls back to a fresh private one,
// and when it cannot make one either it fails closed with an error rather
// than use the rejected path. Only a private directory is remembered, so a
// transient failure (a full temp dir) is retried on the next call.
func vmRuntimeDir() (string, error) {
	runtimeDir.mu.Lock()
	defer runtimeDir.mu.Unlock()
	if runtimeDir.path != "" {
		return runtimeDir.path, nil
	}
	dir, err := resolveRuntimeDir(os.TempDir(), os.Getuid(), os.MkdirTemp)
	if err != nil {
		return "", err
	}
	runtimeDir.path = dir
	return dir, nil
}

// resolveRuntimeDir picks the private runtime directory under base: the
// predictable <base>/jerboa-run-<uid> when it is ours, else a fresh one made
// with mkdirTemp. It never returns a directory that failed checkPrivateDir.
func resolveRuntimeDir(base string, uid int, mkdirTemp func(dir, pattern string) (string, error)) (string, error) {
	dir := expectedRuntimeDir(base, uid)
	_ = os.Mkdir(dir, 0o700)
	err := checkPrivateDir(dir)
	if err == nil {
		return dir, nil
	}
	slog.Warn("vm runtime dir is not private, using a new one", "dir", dir, "err", err)
	fresh, tempErr := mkdirTemp(base, "jerboa-run-")
	if tempErr != nil {
		return "", fmt.Errorf("vm runtime dir: %s is not private (%v) and no private one could be made: %w", dir, err, tempErr)
	}
	if err := checkPrivateDir(fresh); err != nil {
		return "", fmt.Errorf("vm runtime dir: %w", err)
	}
	return fresh, nil
}

func expectedRuntimeDir(base string, uid int) string {
	return filepath.Join(base, fmt.Sprintf("jerboa-run-%d", uid))
}

// qmpRuntimeDir is vmRuntimeDir for code that only names paths in it: the
// socket, config and log helpers, and recovery matching a live VMM's command
// line. Everything that creates or binds an entry there (QEMU and Firecracker
// Start, snapshot restore) calls vmRuntimeDir first and refuses to go on
// without a private directory, so the expected path this returns when none
// could be made is only compared against or cleaned up, never written
// through.
func qmpRuntimeDir() string {
	dir, err := vmRuntimeDir()
	if err != nil {
		return expectedRuntimeDir(os.TempDir(), os.Getuid())
	}
	return dir
}

var runtimeDir struct {
	mu   sync.Mutex
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
