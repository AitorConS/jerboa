//go:build darwin && arm64 && cgo

package vm

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDarwinStatsReadsLiveProcessTree(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		// The shell and every child are in a test-owned process group. Kill the
		// whole group so a failed assertion cannot leave sleep orphaned.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	root, err := readDarwinUsage(cmd.Process.Pid)
	require.NoError(t, err)
	s := &darwinStats{
		pid: cmd.Process.Pid, rootStart: root.start, tree: true,
		vm:      &VM{ID: "live-tree", State: StateRunning},
		lastCPU: make(map[darwinProcessIdentity]uint64),
	}
	require.Eventually(t, func() bool {
		stats := s.Collect()
		return stats.Source == darwinStatsSource+"-tree" && len(s.lastCPU) >= 2 && stats.MemBytes > root.rss
	}, 3*time.Second, 20*time.Millisecond)
}
