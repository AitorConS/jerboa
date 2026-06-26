package network

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// PortForward describes a single host-to-guest port forwarding rule.
type PortForward struct {
	HostPort  uint16
	GuestPort uint16
	Protocol  string
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
	mu        sync.Mutex
	listeners []net.Listener
	wg        sync.WaitGroup
	closed    bool
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
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", pm.HostPort))
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("listen on :%d: %w", pm.HostPort, err)
		}
		target := net.JoinHostPort(guestIP, fmt.Sprintf("%d", pm.GuestPort))
		f.mu.Lock()
		f.listeners = append(f.listeners, ln)
		f.mu.Unlock()
		f.wg.Add(1)
		go f.serve(ln, target)
		slog.Info("port forward started", "host_port", pm.HostPort, "target", target)
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
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
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

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, backend); done <- struct{}{} }()
	// When either direction ends, the deferred Close on both conns unblocks the
	// other copy, so waiting for a single signal is enough.
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
	f.mu.Unlock()

	for _, ln := range listeners {
		_ = ln.Close()
	}
	f.wg.Wait()
}
