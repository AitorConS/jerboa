package config

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

// The config file holds the daemon endpoint and may hold its token. A save
// that fails must leave the previous file, and a damaged file must produce an
// error that names it rather than a client that silently talks elsewhere.

func TestFaults_SaveFailureKeepsPreviousConfig(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jerboa", "config.toml")
			old := &Config{Hypervisor: "qemu", Daemon: DaemonConfig{Endpoint: "tcp://10.0.0.1:7890", Token: "old-token"}}
			require.NoError(t, Save(path, old))

			inj := durabletest.Inject(t, f, 0, nil)
			err := Save(path, &Config{Hypervisor: "firecracker", Daemon: DaemonConfig{Endpoint: "tcp://10.0.0.2:7890", Token: "new-token"}})
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			got, lerr := Load(path)
			require.NoError(t, lerr, "a failed save left a config that does not parse")
			if err != nil && f.Step != durable.StepSyncDir {
				assert.Equal(t, old, got)
			} else {
				assert.Equal(t, "tcp://10.0.0.2:7890", got.Daemon.Endpoint)
				assert.Equal(t, "new-token", got.Daemon.Token)
			}
		})
	}
}

func TestFaults_SaveKeepsTokenOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	path := filepath.Join(t.TempDir(), "jerboa", "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	// A file someone created world-readable before a token was put in it.
	require.NoError(t, os.WriteFile(path, []byte("hypervisor = \"qemu\"\n"), 0o644))

	require.NoError(t, Save(path, &Config{Daemon: DaemonConfig{Token: "secret"}}))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dinfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dinfo.Mode().Perm())
}

func TestFaults_LoadDamagedConfig(t *testing.T) {
	cases := map[string][]byte{
		"truncated table": []byte("[daemon\nendpoint = "),
		"binary":          {0x00, 0x01, 0xff, 0xfe},
		"json instead":    []byte(`{"hypervisor":"qemu"}`),
		"unterminated":    []byte("hypervisor = \"qem"),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, content, 0o600))
			_, err := Load(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), path, "the error must say which file is damaged")
		})
	}
}

func TestFaults_LoadEmptyConfigIsDefaults(t *testing.T) {
	// What a power cut can leave of a file that was being created.
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	got, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, &Config{}, got)
}

func TestFaults_DamagedConfigFallsBackToDefaultEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	t.Setenv("JERBOA_HOST", "")
	path := DefaultPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("[daemon\n"), 0o600))

	// The client still finds the local daemon; it does not crash or dial "".
	assert.Equal(t, DefaultEndpoint(), ResolveEndpoint(""))
	assert.Equal(t, "tcp://1.2.3.4:1", ResolveEndpoint("tcp://1.2.3.4:1"))
}

func TestFaults_SaveIntoUnwritableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "jerboa", "config.toml")
	require.NoError(t, Save(path, &Config{Hypervisor: "qemu"}))
	require.NoError(t, os.Chmod(filepath.Dir(path), 0o500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o700) })

	// Save tightens the directory to 0700 itself, so the write goes through;
	// what matters is that the result is whole either way.
	_ = Save(path, &Config{Hypervisor: "firecracker"})
	got, err := Load(path)
	require.NoError(t, err)
	assert.Contains(t, []string{"qemu", "firecracker"}, got.Hypervisor)
}
