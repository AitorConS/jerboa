package vm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func withStatus(t *testing.T, capAmb string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "status")
	require.NoError(t, os.WriteFile(p, []byte("Name:\tjerboad\nCapInh:\t0000000000003000\nCapAmb:\t"+capAmb+"\n"), 0o600))
	prev := ambientCapsFile
	ambientCapsFile = p
	t.Cleanup(func() { ambientCapsFile = prev })
}

func TestVMMCommandDropsAmbientCapabilities(t *testing.T) {
	withStatus(t, "0000000000003000")
	cmd := vmmCommand(context.Background(), "qemu-system-x86_64", "-m", "256")
	self, err := os.Executable()
	require.NoError(t, err)
	require.Equal(t, self, cmd.Path)
	require.Equal(t, []string{self, VMMExecArg, "qemu-system-x86_64", "-m", "256"}, cmd.Args)
}

func TestVMMCommandWithoutAmbientCapabilities(t *testing.T) {
	withStatus(t, "0000000000000000")
	cmd := vmmCommand(context.Background(), "/usr/bin/true")
	require.Equal(t, "/usr/bin/true", cmd.Path)
	require.Equal(t, []string{"/usr/bin/true"}, cmd.Args)

	ambientCapsFile = filepath.Join(t.TempDir(), "missing")
	require.False(t, hasAmbientCaps())
}

func TestExecVMMNeedsAProgram(t *testing.T) {
	require.Error(t, ExecVMM(nil))
}
