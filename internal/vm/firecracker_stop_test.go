//go:build linux || (darwin && arm64)

package vm

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordingProcess struct{ signaled, killed bool }

func (p *recordingProcess) kill() error { p.killed = true; return nil }

func (p *recordingProcess) signal(os.Signal) error { p.signaled = true; return nil }

// Stopping by name must reach the shutdown endpoint named after the resolved
// ID. An accepted request leaves the VMM running until the guest powers off.
func TestFirecrackerStopByNameUsesResolvedSocket(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "kernel")
	v, err := m.store.Create(Config{Name: "pg-server", Memory: "128M"})
	require.NoError(t, err)
	v.State = StateRunning
	proc := &recordingProcess{}
	v.proc = proc
	var socket string
	m.shutdownAPI = func(path string) error {
		socket = path
		return v.transition(StateStopped)
	}

	require.NoError(t, m.Stop(context.Background(), "pg-server"))
	require.Equal(t, m.shutdownPath(v.ID), socket)
	require.False(t, proc.signaled, "an accepted request must not fall back to SIGTERM")
	require.False(t, proc.killed)
}

// A guest that cannot take the request (a kernel without the vsock power-down
// handler) must not cost the whole grace period: Stop terminates the VMM.
func TestFirecrackerStopGuestRejectsSignalsVMM(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "kernel")
	m.shutdownGrace = 20 * time.Millisecond
	m.shutdownAPI = func(string) error { return errors.New("connection reset") }
	v, err := m.store.Create(Config{Name: "app", Memory: "128M"})
	require.NoError(t, err)
	v.State = StateRunning
	proc := &recordingProcess{}
	v.proc = proc

	start := time.Now()
	require.NoError(t, m.Stop(context.Background(), "app"))
	require.Less(t, time.Since(start), time.Second)
	require.True(t, proc.signaled, "the VMM gets SIGTERM")
	require.True(t, proc.killed, "and SIGKILL once the grace period expires")
}

// A guest that accepts the request but never powers off is killed once the
// grace period expires, without a SIGTERM that would cut its shutdown short.
func TestFirecrackerStopAcceptedButHungIsKilled(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "kernel")
	m.shutdownGrace = 20 * time.Millisecond
	m.shutdownAPI = func(string) error { return nil }
	v, err := m.store.Create(Config{Name: "hung", Memory: "128M"})
	require.NoError(t, err)
	v.State = StateRunning
	proc := &recordingProcess{}
	v.proc = proc

	require.NoError(t, m.Stop(context.Background(), "hung"))
	require.False(t, proc.signaled)
	require.True(t, proc.killed)
}

// fakeVsock emulates Firecracker's host side of a vsock device: it reads the
// CONNECT line and lets reply decide what the "guest" answers.
func fakeVsock(t *testing.T, reply func(c net.Conn, line string)) (string, <-chan string) {
	t.Helper()
	// t.TempDir paths can exceed the 104-byte macOS socket path limit.
	dir, err := os.MkdirTemp("", "fcv")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "v.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	lines := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		lines <- line
		reply(c, line)
	}()
	return path, lines
}

func TestFCRequestPowerdownAccepted(t *testing.T) {
	path, lines := fakeVsock(t, func(c net.Conn, _ string) { _, _ = c.Write([]byte("OK 1073741824\n")) })
	require.NoError(t, fcRequestPowerdown(path))
	require.Equal(t, "CONNECT 1245859927\n", <-lines, "port must match VIRTIO_SOCK_POWERDOWN_PORT")
}

// Firecracker closes the host stream without a reply when the guest resets the
// connection, which is what a kernel without the handler does.
func TestFCRequestPowerdownRejected(t *testing.T) {
	path, _ := fakeVsock(t, func(net.Conn, string) {})
	require.Error(t, fcRequestPowerdown(path))
}

// A guest whose vsock driver never answers must not block Stop.
func TestFCRequestPowerdownSilentGuestTimesOut(t *testing.T) {
	old := fcVsockHandshakeTimeout
	fcVsockHandshakeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { fcVsockHandshakeTimeout = old })
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	path, _ := fakeVsock(t, func(net.Conn, string) { <-done })
	start := time.Now()
	require.Error(t, fcRequestPowerdown(path))
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestFCRequestPowerdownUnexpectedReply(t *testing.T) {
	path, _ := fakeVsock(t, func(c net.Conn, _ string) { _, _ = c.Write([]byte("ERR nope\n")) })
	require.ErrorContains(t, fcRequestPowerdown(path), "unexpected reply")
}

func TestFCRequestPowerdownNoSocket(t *testing.T) {
	require.Error(t, fcRequestPowerdown(filepath.Join(t.TempDir(), "missing.sock")))
}
