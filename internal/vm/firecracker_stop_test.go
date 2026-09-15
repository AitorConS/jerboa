//go:build linux || (darwin && arm64)

package vm

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

type recordingProcess struct{ signaled, killed bool }

func (p *recordingProcess) kill() error { p.killed = true; return nil }

func (p *recordingProcess) signal(os.Signal) error { p.signaled = true; return nil }

// Stopping by name must reach the API socket named after the resolved ID.
// A miss falls back to SIGTERM of the VMM, which skips guest shutdown.
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
	require.Equal(t, m.vmSockPath(v.ID), socket)
	require.False(t, proc.signaled, "reachable API must not fall back to SIGTERM")
	require.False(t, proc.killed)
}
