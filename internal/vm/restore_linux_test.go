package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func accepts(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// A VM that outlived the previous daemon gets its published ports back, and
// loses them, with the files its VMM left, when the VMM exits. Before, a
// re-adopted VM was listed as running with ports that refused connections.
func TestRestoreAdoptedPublishesPortsAgain(t *testing.T) {
	store := NewMemoryStore()
	port := freeTCPPort(t)
	adopted, err := store.Create(Config{
		ImagePath: "x.img", Memory: "256M", IPAddress: "127.0.0.1",
		PortMaps: []PortMap{{HostPort: uint16(port), GuestPort: 8080, Protocol: "tcp", BindAddr: "127.0.0.1"}}, //nolint:gosec // a port number
	})
	require.NoError(t, err)
	self, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	adopted.State = StateRunning
	adopted.proc = &osProcess{self}
	adopted.pid = os.Getpid()

	// Not adopted: stopped, and running without a process of its own.
	otherPort := freeTCPPort(t)
	stopped, err := store.Create(Config{
		ImagePath: "x.img", Memory: "256M", IPAddress: "127.0.0.1",
		PortMaps: []PortMap{{HostPort: uint16(otherPort), GuestPort: 8080, Protocol: "tcp", BindAddr: "127.0.0.1"}}, //nolint:gosec // a port number
	})
	require.NoError(t, err)
	stopped.State = StateStopped

	leftover := filepath.Join(t.TempDir(), "fc-"+adopted.ID+".sock")
	require.NoError(t, os.WriteFile(leftover, nil, 0o600))
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	require.False(t, accepts(addr))

	restoreAdopted(context.Background(), store.List(), nil, func(id string) []string {
		if id == adopted.ID {
			return []string{leftover}
		}
		return nil
	})

	require.True(t, accepts(addr), "the published port of a re-adopted VM must accept connections again")
	require.False(t, accepts(fmt.Sprintf("127.0.0.1:%d", otherPort)), "a stopped VM publishes nothing")
	require.Nil(t, stopped.hostCleanup)

	// What the adopt monitor runs when the VMM is gone.
	adopted.mu.RLock()
	cleanup := adopted.hostCleanup
	adopted.mu.RUnlock()
	require.NotNil(t, cleanup)
	cleanup()
	require.False(t, accepts(addr), "the port must be released with the VM")
	require.NoFileExists(t, leftover)
}
