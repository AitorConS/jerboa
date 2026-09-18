//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordingProcess struct{ signaled, killed bool }

func (p *recordingProcess) kill() error { p.killed = true; return nil }

func (p *recordingProcess) signal(os.Signal) error { p.signaled = true; return nil }

// Stopping by name must reach the API socket named after the resolved ID.
// A miss falls back to SIGTERM of the VMM, which skips guest shutdown.
func TestFirecrackerStopByNameUsesResolvedSocket(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "kernel")
	m.guestShutdown = true
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
	require.Equal(t, m.vmSockPath(v.ID), socket)
	require.False(t, proc.signaled, "reachable API must not fall back to SIGTERM")
	require.False(t, proc.killed)
}

// Where the guest cannot act on the VMM's shutdown request (upstream
// Firecracker offers only SendCtrlAltDel, which Nanos ignores), Stop must
// terminate the VMM instead of waiting out the whole grace period.
func TestFirecrackerStopWithoutGuestShutdownSignalsVMM(t *testing.T) {
	m := NewFirecrackerManager("firecracker", "kernel")
	m.guestShutdown = false // upstream Firecracker; the macOS VMM sets it
	m.shutdownGrace = 20 * time.Millisecond
	called := false
	m.shutdownAPI = func(string) error { called = true; return nil }
	v, err := m.store.Create(Config{Name: "app", Memory: "128M"})
	require.NoError(t, err)
	v.State = StateRunning
	proc := &recordingProcess{}
	v.proc = proc

	start := time.Now()
	require.NoError(t, m.Stop(context.Background(), "app"))
	require.Less(t, time.Since(start), time.Second, "must not wait out a 30s grace period")
	require.False(t, called, "a request the guest ignores must not be sent")
	require.True(t, proc.signaled, "the VMM gets SIGTERM")
	require.True(t, proc.killed, "and SIGKILL once the grace period expires")
}
