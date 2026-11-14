package credentials

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The credentials file is the user's logins to every registry. A login or
// logout that fails half-way must not cost the other logins.

func home(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv(EnvToken, "")
	return dir
}

func TestFaults_SaveFailureKeepsEveryLogin(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			home(t)
			old := File{Registries: map[string]Entry{
				"registry.example": {Token: "jpat_old", Login: "ana"},
				"other.example":    {Token: "jpat_other"},
			}}
			require.NoError(t, old.Save())

			next := File{Registries: map[string]Entry{"registry.example": {Token: "jpat_new", Login: "ana"}}}
			inj := durabletest.Inject(t, f, 0, nil)
			err := next.Save()
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			got, lerr := Load()
			require.NoError(t, lerr, "a failed save left credentials that do not parse")
			if err != nil && f.Step != durable.StepSyncDir {
				assert.Equal(t, old, got)
			} else {
				assert.Equal(t, next, got)
			}
		})
	}
}

func TestFaults_CredentialsStayOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := home(t)
	p := filepath.Join(dir, ".jerboa", "credentials.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(`{"registries":{}}`), 0o644))

	f := File{Registries: map[string]Entry{"registry.example": {Token: "jpat_x"}}}
	for range 3 {
		require.NoError(t, f.Save())
	}
	info, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestFaults_NoTemporaryFileHoldsATokenAfterAFailure(t *testing.T) {
	dir := home(t)
	f := File{Registries: map[string]Entry{"registry.example": {Token: "jpat_secret"}}}
	durabletest.Inject(t, durabletest.Failure{Step: durable.StepRename, Err: os.ErrPermission}, 0, nil)
	require.Error(t, f.Save())
	durable.SetFault(nil)

	entries, err := os.ReadDir(filepath.Join(dir, ".jerboa"))
	require.NoError(t, err)
	assert.Empty(t, entries, "a failed save left a copy of the token on disk")
}

func TestFaults_LoadDamagedCredentials(t *testing.T) {
	cases := map[string][]byte{
		"truncated": []byte(`{"registries":{"registry.example":{"tok`),
		"garbage":   {0xff, 0x00},
		"array":     []byte(`[]`),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := home(t)
			p := filepath.Join(dir, ".jerboa", "credentials.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
			require.NoError(t, os.WriteFile(p, content, 0o600))

			got, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), p, "the error must say which file is damaged")
			assert.NotNil(t, got.Registries, "callers index the map even after an error")
		})
	}
}

func TestFaults_EmptyFileIsNoLogins(t *testing.T) {
	for _, content := range [][]byte{nil, []byte("\n"), []byte("  \n\t")} {
		dir := home(t)
		p := filepath.Join(dir, ".jerboa", "credentials.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, content, 0o600))
		got, err := Load()
		require.NoError(t, err)
		assert.Empty(t, got.Registries)
		got.Registries["registry.example"] = Entry{Token: "jpat_x"}
		require.NoError(t, got.Save(), "logging in again must work")
	}
}

func TestFaults_LoadNullRegistries(t *testing.T) {
	dir := home(t)
	p := filepath.Join(dir, ".jerboa", "credentials.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(`{"registries":null}`), 0o600))
	got, err := Load()
	require.NoError(t, err)
	require.NotNil(t, got.Registries)
	got.Registries["registry.example"] = Entry{Token: "jpat_x"}
	require.NoError(t, got.Save())
}

func TestFaults_SaveCreatesPrivateDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := home(t)
	require.NoError(t, File{Registries: map[string]Entry{"r": {Token: "t"}}}.Save())
	info, err := os.Stat(filepath.Join(dir, ".jerboa"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}
