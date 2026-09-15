//go:build linux || darwin

package builder

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditPythonRemovesOldDependenciesAndPreservesOnFailure(t *testing.T) {
	tools := t.TempDir()
	pip := filepath.Join(tools, "pip")
	require.NoError(t, os.WriteFile(pip, []byte(`#!/bin/sh
while [ "$1" != "--target" ]; do shift; done
shift
if grep -q fail requirements.txt; then exit 1; fi
if grep -q colorama requirements.txt; then mkdir -p "$1/colorama"; fi
`), 0o755))
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	dir := t.TempDir()
	req := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(req, []byte("colorama"), 0o600))
	driver := &PythonDriver{}
	_, err := driver.Build(context.Background(), dir, Options{})
	require.NoError(t, err)
	require.DirExists(t, filepath.Join(dir, "packages", "colorama"))
	require.NoError(t, os.WriteFile(req, []byte("fail"), 0o600))
	_, err = driver.Build(context.Background(), dir, Options{})
	require.Error(t, err)
	require.DirExists(t, filepath.Join(dir, "packages", "colorama"))
	require.NoError(t, os.WriteFile(req, nil, 0o600))
	_, err = driver.Build(context.Background(), dir, Options{})
	require.NoError(t, err)
	require.NoDirExists(t, filepath.Join(dir, "packages", "colorama"))
}
func TestAuditNodeInvalidatesOnManifestChange(t *testing.T) {
	tools := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tools, "npm"), []byte("#!/bin/sh\necho install >> calls\nmkdir -p node_modules\n"), 0o755))
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	dir := t.TempDir()
	p := filepath.Join(dir, "package.json")
	require.NoError(t, os.WriteFile(p, []byte("{}"), 0o600))
	driver := &NodeDriver{}
	_, err := driver.Build(context.Background(), dir, Options{})
	require.NoError(t, err)
	_, err = driver.Build(context.Background(), dir, Options{})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, []byte(`{"dependencies":{"a":"1"}}`), 0o600))
	_, err = driver.Build(context.Background(), dir, Options{})
	require.NoError(t, err)
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	require.NoError(t, err)
	require.Equal(t, "install\ninstall\n", string(calls))
}
