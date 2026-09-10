package network

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PortForward describes a single host-to-guest port forwarding rule.
type PortForward struct {
	HostPort  uint16
	GuestPort uint16
	Protocol  string
	// BindAddr is the host address to listen on. Empty publishes on all
	// interfaces (reachable from the LAN, and mirrored to the Windows host by
	// WSL2); set it to "127.0.0.1" to restrict a port to the local host.
	BindAddr string
}

// Forwarder publishes guest ports on the host by proxying real listening
// sockets to the guest over the bridge.
//
// It replaces iptables DNAT, which never worked for host-local access (the
// PREROUTING rule matched the wrong direction) and is invisible to WSL2's
// localhost forwarding (a DNAT rule opens no listening socket, so a Windows
// host cannot reach it). A userspace listener is a normal socket: reachable
// from the host itself and mirrored by WSL2 to the Windows side.
type Forwarder struct {
	mu          sync.Mutex
	listeners   []net.Listener
	wg          sync.WaitGroup
	closed      bool
	connections map[net.Conn]struct{}
}

// dialTimeout bounds how long a proxied connection waits to reach the guest.
const dialTimeout = 10 * time.Second

// StartForwarder opens a TCP listener for each TCP port map and proxies accepted
// connections to guestIP. UDP maps are not yet supported and are skipped with a
// warning. The returned Forwarder must be closed when the VM stops.
func StartForwarder(guestIP string, ports []PortForward) (*Forwarder, error) {
	if guestIP == "" {
		return nil, fmt.Errorf("guest IP is required for port forwarding")
	}
	f := &Forwarder{}
	for _, pm := range ports {
		if strings.EqualFold(pm.Protocol, "udp") {
			slog.Warn("port forward: UDP is not supported yet, skipping", "host_port", pm.HostPort)
			continue
		}
		listenAddr := net.JoinHostPort(pm.BindAddr, strconv.Itoa(int(pm.HostPort)))
		ln, err := net.Listen("tcp", listenAddr)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("listen on %s: %w", listenAddr, err)
		}
		target := net.JoinHostPort(guestIP, fmt.Sprintf("%d", pm.GuestPort))
		f.mu.Lock()
		f.listeners = append(f.listeners, ln)
		f.mu.Unlock()
		f.wg.Add(1)
		go f.serve(ln, target)
		slog.Info("port forward started", "listen", listenAddr, "target", target)
	}
	return f, nil
}

// serve accepts connections on ln until it is closed and proxies each to target.
func (f *Forwarder) serve(ln net.Listener, target string) {
	defer f.wg.Done()
	for {
		client, err := ln.Accept()
		if err != nil {
			return // listener closed: stop serving
		}
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			_ = client.Close()
			return
		}
		if f.connections == nil {
			f.connections = make(map[net.Conn]struct{})
		}
		f.connections[client] = struct{}{}
		f.wg.Add(1)
		f.mu.Unlock()
		go func() {
			defer f.wg.Done()
			defer func() { f.mu.Lock(); delete(f.connections, client); f.mu.Unlock() }()
			proxyConn(client, target)
		}()
	}
}

// proxyConn dials the guest target and copies bytes in both directions until
// either side closes.
func proxyConn(client net.Conn, target string) {
	defer client.Close()
	backend, err := net.DialTimeout("tcp", target, dialTimeout)
	if err != nil {
		slog.Debug("port forward: dial guest failed", "target", target, "err", err)
		return
	}
	defer backend.Close()

	// Each direction closes both conns when it finishes so the other io.Copy
	// unblocks and returns promptly. proxyConn then waits for BOTH goroutines
	// before returning: it runs under the Forwarder's WaitGroup, so draining
	// both is what lets Close()'s wg.Wait() guarantee no copy is still writing to
	// a socket after Close returns. (Waiting on a single signal would leave the
	// other copy running untracked; closing the conns only inside a deferred
	// return — as before — would deadlock this two-signal wait.)
	copyDir := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = client.Close()
		_ = backend.Close()
	}
	done := make(chan struct{}, 2)
	go func() { copyDir(backend, client); done <- struct{}{} }()
	go func() { copyDir(client, backend); done <- struct{}{} }()
	<-done
	<-done
}

// Close stops all listeners and waits for in-flight connections to drain.
func (f *Forwarder) Close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	listeners := f.listeners
	f.listeners = nil
	clients := make([]net.Conn, 0, len(f.connections))
	for client := range f.connections {
		clients = append(clients, client)
	}
	f.mu.Unlock()

	for _, ln := range listeners {
		_ = ln.Close()
	}
	for _, client := range clients {
		_ = client.Close()
	}
	f.wg.Wait()
}
