package network

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// startEchoServer starts a TCP echo server and returns its address and a stop func.
func startEchoServer(t *testing.T) (host string, port uint16, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", uint16(addr.Port), func() { _ = ln.Close() }
}

// freeTCPPort returns an available localhost TCP port.
func freeTCPPort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func TestForwarderProxiesTCP(t *testing.T) {
	guestHost, guestPort, stopEcho := startEchoServer(t)
	defer stopEcho()

	hostPort := freeTCPPort(t)
	fwd, err := StartForwarder(guestHost, []PortForward{{HostPort: hostPort, GuestPort: guestPort, Protocol: "tcp"}})
	if err != nil {
		t.Fatalf("StartForwarder: %v", err)
	}
	defer fwd.Close()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), 2*time.Second)
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello unikernel")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("got %q, want %q", buf, msg)
	}
}

func TestForwarderRequiresGuestIP(t *testing.T) {
	if _, err := StartForwarder("", []PortForward{{HostPort: 1, GuestPort: 1}}); err == nil {
		t.Fatal("expected error for empty guest IP")
	}
}

func TestForwarderCloseStopsListener(t *testing.T) {
	hostPort := freeTCPPort(t)
	fwd, err := StartForwarder("127.0.0.1", []PortForward{{HostPort: hostPort, GuestPort: hostPort}})
	if err != nil {
		t.Fatalf("StartForwarder: %v", err)
	}
	fwd.Close()

	// After Close, the host port must be free to bind again.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		t.Fatalf("port still bound after Close: %v", err)
	}
	_ = ln.Close()

	fwd.Close() // idempotent
}

func TestForwarderBindAddrRestrictsToLoopback(t *testing.T) {
	guestHost, guestPort, stopEcho := startEchoServer(t)
	defer stopEcho()

	hostPort := freeTCPPort(t)
	fwd, err := StartForwarder(guestHost, []PortForward{
		{HostPort: hostPort, GuestPort: guestPort, Protocol: "tcp", BindAddr: "127.0.0.1"},
	})
	if err != nil {
		t.Fatalf("StartForwarder: %v", err)
	}
	defer fwd.Close()

	// The listener must be bound to 127.0.0.1 specifically, not 0.0.0.0.
	if len(fwd.listeners) != 1 {
		t.Fatalf("expected 1 listener, got %d", len(fwd.listeners))
	}
	got := fwd.listeners[0].Addr().(*net.TCPAddr).IP.String()
	if got != "127.0.0.1" {
		t.Fatalf("listener bound to %s, want 127.0.0.1", got)
	}
}

func TestForwarderSkipsUDP(t *testing.T) {
	// UDP is not supported yet: it is skipped, so no listener is opened and
	// Start succeeds without error.
	fwd, err := StartForwarder("127.0.0.1", []PortForward{{HostPort: freeTCPPort(t), GuestPort: 53, Protocol: "udp"}})
	if err != nil {
		t.Fatalf("StartForwarder: %v", err)
	}
	defer fwd.Close()
	if len(fwd.listeners) != 0 {
		t.Fatalf("expected no listeners for UDP-only map, got %d", len(fwd.listeners))
	}
}

func TestForwarderCloseWithIdleConnection(t *testing.T) {
	host, port, stop := startEchoServer(t)
	defer stop()
	f, err := StartForwarder(host, []PortForward{{HostPort: 0, GuestPort: port, BindAddr: "127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", f.listeners[0].Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(conn, b); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { f.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked on idle connection")
	}
}
