package childproc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestCommandDropsAmbientCapabilities(t *testing.T) {
	withStatus(t, "0000000000003000")
	cmd := Command(context.Background(), "qemu-system-x86_64", "-m", "256")
	self, err := os.Executable()
	require.NoError(t, err)
	require.Equal(t, self, cmd.Path)
	require.Equal(t, []string{self, ReexecArg, "qemu-system-x86_64", "-m", "256"}, cmd.Args)
}

func TestCommandWithoutAmbientCapabilities(t *testing.T) {
	withStatus(t, "0000000000000000")
	cmd := Command(context.Background(), "/usr/bin/true")
	require.Equal(t, "/usr/bin/true", cmd.Path)
	require.Equal(t, []string{"/usr/bin/true"}, cmd.Args)

	ambientCapsFile = filepath.Join(t.TempDir(), "missing")
	require.False(t, hasAmbientCaps())
}

// The re-exec carries the program's environment, not the daemon's: Exec
// passes on os.Environ of the re-executed process, which is cmd.Env.
func TestCommandEnvIsMinimalThroughReexec(t *testing.T) {
	withStatus(t, "0000000000003000")
	t.Setenv("JERBOA_AUTH_TOKEN", "s3cr3t")
	cmd := Command(context.Background(), "mkfs", "out.img")
	require.Equal(t, ReexecArg, cmd.Args[1])
	require.NotNil(t, cmd.Env)
	for _, kv := range cmd.Env {
		require.False(t, strings.HasPrefix(kv, "JERBOA_"), "child env has %q", kv)
	}
}

func TestExecNeedsAProgram(t *testing.T) {
	require.Error(t, Exec(nil))
}
