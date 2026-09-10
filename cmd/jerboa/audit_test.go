package main

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/compose"
	"github.com/stretchr/testify/require"
)

func TestAuditComposeStateIsolation(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yml")
	b := filepath.Join(dir, "b.yml")
	require.NotEqual(t, stateFilePath(a), stateFilePath(b))
	require.NoError(t, writeState(a, compose.State{Services: map[string]string{"a": "vm-a"}}))
	require.NoError(t, writeState(b, compose.State{Services: map[string]string{"b": "vm-b"}}))
	require.NoError(t, removeState(a))
	state, err := readState(b)
	require.NoError(t, err)
	require.Equal(t, "vm-b", state.Services["b"])
}
func TestAuditHealthPortRange(t *testing.T) {
	for _, s := range []string{"tcp:0", "tcp:99999", "http:-1:/"} {
		_, err := parseHealthCheck(s)
		require.Error(t, err)
	}
}
func TestAuditEnvFlagWins(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(p, []byte("A=file\nB=keep\n"), 0o600))
	env, err := buildEnv([]string{"A=flag", "A=last"}, p)
	require.NoError(t, err)
	require.Equal(t, []string{"A=last", "B=keep"}, env)
}
func TestAuditRootfsValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rootfs.tar")
	require.Error(t, validateRootfs(p))
	require.NoError(t, os.WriteFile(p, []byte("invalid"), 0o600))
	require.Error(t, validateRootfs(p))
	f, err := os.Create(p)
	require.NoError(t, err)
	tw := tar.NewWriter(f)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "etc/issue", Mode: 0o644, Size: 2}))
	_, err = tw.Write([]byte("ok"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, f.Close())
	require.NoError(t, validateRootfs(p))
}
