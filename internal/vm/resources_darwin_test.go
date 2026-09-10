//go:build darwin && cgo

package vm

import (
	"github.com/stretchr/testify/require"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestNativeMemoryWatchdogKillsOnlyItsProcess(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	v := &VM{ID: "watchdog-test", State: StateRunning, Cfg: Config{MemoryMax: 1}, proc: &osProcess{cmd.Process}}
	require.NoError(t, defaultApplyLimits(v, cmd.Process.Pid))
	t.Cleanup(func() { _ = v.cgroupMgr.Remove() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not stop process")
	}
	require.Contains(t, v.Warnings()[0], "overshoot")
}
func TestNativeCPUWeightMapsToOSPriority(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	v := &VM{ID: "priority-test", Cfg: Config{CPUShares: 100}}
	require.NoError(t, defaultApplyLimits(v, cmd.Process.Pid))
	nice, err := syscall.Getpriority(syscall.PRIO_PROCESS, cmd.Process.Pid)
	require.NoError(t, err)
	require.Equal(t, nativeNice(100), nice)
	require.Greater(t, nativeNice(1), nativeNice(100))
	require.Greater(t, nativeNice(100), nativeNice(10000))
}
