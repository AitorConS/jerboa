package upgradetest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A go command run with the fixture's home leaves a module cache of
// read-only directories; restoring over it must still work.
func TestRemoveTreeEmptiesReadOnlyDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fixture")
	dir := filepath.Join(root, "home", "go", "pkg", "mod", "example.com", "m@v1.0.0")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "m.go"), []byte("package m\n"), 0o400))
	require.NoError(t, os.Chmod(dir, 0o500))               //nolint:gosec // a directory of this test
	require.NoError(t, os.Chmod(filepath.Dir(dir), 0o500)) //nolint:gosec // a directory of this test
	t.Cleanup(func() { _ = removeTree(root) })

	if os.RemoveAll(root) == nil {
		t.Skip("this platform or user removes read-only directories")
	}
	require.NoError(t, removeTree(root))
	require.NoDirExists(t, root)
}
