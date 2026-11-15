//go:build linux

package vm

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
)

// The Firecracker counterpart of TestFaults_RunningStateThatCannotBeSaved: a
// VMM whose running state cannot be recorded must not be left running. Linux
// only, where the manager waits for no API socket and a helper process can
// stand in for the VMM.
func TestFaults_FirecrackerRunningStateThatCannotBeSaved(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			dir := t.TempDir()
			image := filepath.Join(t.TempDir(), "base.img")
			require.NoError(t, os.WriteFile(image, []byte("a root filesystem"), 0o600))
			store := restored(t, dir)
			mgr := NewFirecrackerManager("fake-firecracker", "/kernel",
				WithFCCommandFunc(fakeVMM("block")), WithFCStore(store), WithFCDiskDir(t.TempDir()))
			v, err := mgr.Create(context.Background(), Config{ImagePath: image, Memory: "256M"})
			require.NoError(t, err)

			statePath := filepath.Join(dir, v.ID, "state.json")
			stopRecorded := func() bool {
				data, err := os.ReadFile(statePath)
				var st vmState
				return err == nil && json.Unmarshal(data, &st) == nil && st.State == StateStopped
			}
			// The monitor records the stop on its own goroutine; wait for
			// that write so it does not land while the directory is removed.
			t.Cleanup(func() {
				_ = mgr.Kill(context.Background(), v.ID)
				deadline := time.Now().Add(10 * time.Second)
				for !stopRecorded() && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
			})

			// Only the registry's own write is failed: the start writes
			// other files (the VMM config) that are not under test here.
			durabletest.Inject(t, f, 0, func(path string) bool { return strings.HasPrefix(path, filepath.Join(dir, v.ID)) })
			err = mgr.Start(context.Background(), v.ID)
			durable.SetFault(nil)

			if err != nil {
				require.ErrorContains(t, err, "record running state")
				waitDone(t, v)
				requireReaped(t, vmPID(v))
				assert.Equal(t, StateStopped, v.GetState())
				require.Eventually(t, stopRecorded, 10*time.Second, 5*time.Millisecond, "the registry never recorded the stop")
				return
			}
			var st vmState
			readJSONFile(t, statePath, &st)
			assert.Equal(t, StateRunning, st.State)
			assert.Equal(t, vmPID(v), st.PID)
		})
	}
}

// An attach-mode Firecracker VM whose console nobody reads is still reaped,
// and the monitor's exit diagnostic into the console does not block either.
func TestFirecracker_AttachWithoutReader_StillStops(t *testing.T) {
	image := filepath.Join(t.TempDir(), "base.img")
	require.NoError(t, os.WriteFile(image, []byte("a root filesystem"), 0o600))
	mgr := NewFirecrackerManager("fake-firecracker", "/kernel",
		WithFCCommandFunc(func(context.Context, string, ...string) *exec.Cmd {
			return exec.Command("sh", "-c", "head -c 4000000 /dev/zero; sleep 0.2; exit 3")
		}), WithFCDiskDir(t.TempDir()))
	v, err := mgr.Create(context.Background(), Config{ImagePath: image, Memory: "256M", Attach: true})
	require.NoError(t, err)
	_ = mgr.Start(context.Background(), v.ID) // the VMM may exit before or after Start returns
	select {
	case <-v.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("attach-mode VM never reached stopped with no console reader")
	}
	require.Equal(t, StateStopped, v.GetState())
}
