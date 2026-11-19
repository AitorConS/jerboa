package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/childproc"
	"github.com/stretchr/testify/require"
)

// mkfs reads packages built from someone else's files: on Linux it must not
// keep the daemon's ambient capabilities, so it is started through the same
// re-exec as the VMMs.
func TestDirectFunc_DropsAmbientCapabilities(t *testing.T) {
	status := filepath.Join(t.TempDir(), "status")
	require.NoError(t, os.WriteFile(status, []byte("Name:\tjerboad\nCapAmb:\t0000000000003000\n"), 0o600))
	t.Cleanup(childproc.SetStatusFileForTest(status))
	t.Setenv("JERBOA_AUTH_TOKEN", "s3cr3t")

	cmd := directFunc("/opt/tools/mkfs", "/tmp/boot.img", "/tmp/kernel.img")(context.Background(), "/tmp/disk.img", "app", "")
	self, err := os.Executable()
	require.NoError(t, err)
	require.Equal(t, self, cmd.Path)
	require.Equal(t, []string{self, childproc.ReexecArg, "/opt/tools/mkfs", "-b", "/tmp/boot.img", "-k", "/tmp/kernel.img", "/tmp/disk.img"}, cmd.Args)
	require.NotNil(t, cmd.Env)
	for _, kv := range cmd.Env {
		require.NotContains(t, kv, "s3cr3t")
	}
}
