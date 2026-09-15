//go:build linux

package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// Linux uses the upstream Firecracker command, config and process lifecycle.
func platformInitFC(_ *FirecrackerManager)                                    {}
func (m *FirecrackerManager) checkFCConfig(_ context.Context, _ string) error { return nil }

func (m *FirecrackerManager) validateFCPlatform(_ Config) error { return nil }
func (m *FirecrackerManager) writeNativeFCConfig(_ string, _ Config, _ string) (string, error) {
	panic("native Firecracker config is only available on macOS")
}
func cleanupFCConfig(path string)                    { _ = os.Remove(path) }
func prepareNativeFCHealth(_ *VM)                    {}
func awaitFCReady(_ context.Context, _ string) error { return nil }
func waitFCProcess(cmd *exec.Cmd, _ string) error {
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("wait for firecracker: %w", err)
	}
	return nil
}

func (m *FirecrackerManager) prepareFCHost(_ *VM) (func(), error) { return nil, nil }

func WithFCGuestDNS(fn func([]byte, string) ([]byte, error)) FCOption {
	return func(m *FirecrackerManager) { m.guestDNS = fn }
}
func (m *FirecrackerManager) GuestDNSUpstream() (string, error) { return "", nil }
