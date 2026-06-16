package vm

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// qmpDo connects to a QEMU Machine Protocol socket at addr (TCP "host:port"),
// negotiates capabilities, and executes the given QMP command.
// Using TCP makes this work identically on Linux, macOS, and Windows.
func qmpDo(addr, command string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
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

// freePort returns an available TCP port on the loopback interface.
// There is a small race between Close and QEMU binding; acceptable in practice.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
