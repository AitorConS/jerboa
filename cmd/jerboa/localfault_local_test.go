package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/builder"
	"github.com/AitorConS/jerboa/internal/config"
	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/AitorConS/jerboa/internal/volume"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- jerboa config ---

func lfConfigPath(home string) string { return filepath.Join(home, ".jerboa", "config.toml") }

// A damaged config file stops config get and set with an error naming the
// file. set must not write a fresh file over it: the file may hold the
// daemon endpoint and token, which the damage does not make worthless.
func TestLocalFault_ConfigFileDamaged(t *testing.T) {
	for name, content := range map[string]string{"not toml": "[daemon\nendpoint = ", "binary": "\x00\xff\xfe", "wrong type": "hypervisor = [1, 2]\n"} {
		t.Run(name, func(t *testing.T) {
			home := fdIsolate(t)
			path := lfWrite(t, lfConfigPath(home), content)
			for _, args := range [][]string{{"config", "get", "hypervisor"}, {"config", "set", "hypervisor", "qemu"}} {
				res := fdExec(t, "", args...)
				fdRequireCleanFailure(t, res, path)
				assert.Empty(t, res.stdout)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, content, string(data), "%v rewrote a damaged config file", args)
			}
		})
	}
}

// Setting one key keeps every other setting, and a value or key that is not
// known changes nothing.
func TestLocalFault_ConfigSetKeepsOtherSettings(t *testing.T) {
	home := fdIsolate(t)
	original := "[daemon]\nendpoint = \"tcp://10.0.0.5:7890\"\ntoken = \"daemon-token\"\n"
	path := lfWrite(t, lfConfigPath(home), original)

	for _, args := range [][]string{
		{"config", "set", "hypervisor", "xen"},
		{"config", "set", "hypervisor", ""},
		{"config", "set", "nosuchkey", "1"},
		{"config", "set", "", ""},
		{"config", "get", "nosuchkey"},
		{"config", "get", "daemon.token"},
	} {
		res := fdExec(t, "", args...)
		fdRequireCleanFailure(t, res)
		assert.NotContains(t, res.stdout+res.err.Error(), "daemon-token", "a stored token was printed")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, original, string(data), "%v changed the config file", args)
	}

	res := fdExec(t, "", "config", "set", "Hypervisor", "QEMU")
	require.NoError(t, res.err)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "qemu", cfg.Hypervisor)
	assert.Equal(t, "tcp://10.0.0.5:7890", cfg.Daemon.Endpoint)
	assert.Equal(t, "daemon-token", cfg.Daemon.Token)

	res = fdExec(t, "", "config", "get", "hypervisor")
	require.NoError(t, res.err)
	assert.Equal(t, "qemu\n", res.stdout)

	// auto is the absence of a choice, and reads back as auto.
	require.NoError(t, fdExec(t, "", "config", "set", "hypervisor", "auto").err)
	res = fdExec(t, "", "config", "get", "hypervisor")
	require.NoError(t, res.err)
	assert.Equal(t, "auto\n", res.stdout)
}

// The disk fills up, errors or the process dies while the config is saved:
// the file holds the old settings or the new ones, never a mix or nothing,
// and the daemon's endpoint and token survive either way.
func TestLocalFault_ConfigSetSaveFailures(t *testing.T) {
	for _, f := range durabletest.Failures() {
		t.Run(f.Name, func(t *testing.T) {
			home := fdIsolate(t)
			path := lfWrite(t, lfConfigPath(home), "hypervisor = \"firecracker\"\n\n[daemon]\ntoken = \"daemon-token\"\n")
			inj := durabletest.Inject(t, f, 0, func(p string) bool { return filepath.Base(p) == "config.toml" })
			res := fdExec(t, "", "config", "set", "hypervisor", "qemu")
			require.True(t, inj.Fired())
			durable.SetFault(nil)
			require.Nil(t, res.panicked)

			cfg, err := config.Load(path)
			require.NoError(t, err, "a failed save left a config file that does not parse")
			assert.Equal(t, "daemon-token", cfg.Daemon.Token)
			if res.err != nil && f.Step != durable.StepSyncDir {
				assert.Equal(t, "firecracker", cfg.Hypervisor)
				assert.NotContains(t, res.stdout, "hypervisor = qemu", "a save that failed was reported as done")
			} else {
				assert.Equal(t, "qemu", cfg.Hypervisor)
			}
		})
	}
}

// --- jerboa init ---

func TestLocalFault_Init(t *testing.T) {
	t.Run("directory does not exist", func(t *testing.T) {
		fdIsolate(t)
		res := fdExec(t, "", "init", filepath.Join(t.TempDir(), "absent"))
		fdRequireCleanFailure(t, res, "init")
	})
	t.Run("path is a file", func(t *testing.T) {
		fdIsolate(t)
		file := lfWrite(t, filepath.Join(t.TempDir(), "file"), "x")
		res := fdExec(t, "", "init", file)
		fdRequireCleanFailure(t, res, "init")
	})
	t.Run("unknown language", func(t *testing.T) {
		fdIsolate(t)
		dir := t.TempDir()
		res := fdExec(t, "", "init", "--lang", "cobol", dir)
		fdRequireCleanFailure(t, res, "cobol")
		_, err := os.Stat(filepath.Join(dir, builder.ConfigFileName))
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	t.Run("existing file is kept unless forced", func(t *testing.T) {
		fdIsolate(t)
		dir := t.TempDir()
		path := lfWrite(t, filepath.Join(dir, builder.ConfigFileName), "# mine\n")
		res := fdExec(t, "", "init", "--lang", "go", dir)
		fdRequireCleanFailure(t, res, "--force")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "# mine\n", string(data))

		require.NoError(t, fdExec(t, "", "init", "--lang", "go", "--force", dir).err)
		cfg, err := builder.LoadConfig(dir)
		require.NoError(t, err, "the generated file must be one the build accepts")
		assert.Equal(t, "go", cfg.Build.Lang)
	})
	t.Run("directory is read-only", func(t *testing.T) {
		lfSkipWithoutPermissions(t)
		fdIsolate(t)
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		res := fdExec(t, "", "init", "--lang", "go", dir)
		fdRequireCleanFailure(t, res, "init: write")
		assert.NotContains(t, res.stdout, "Created")
	})
}

// --- jerboa volume migrate ---

// lfLegacyVolume creates a volume in the client-side store next to an image
// store at <root>/images, as older CLIs did, and returns the image store
// path to pass as --store.
func lfLegacyVolume(t *testing.T, name, content string) (storeFlag string, disk string) {
	t.Helper()
	root := t.TempDir()
	store, err := volume.NewStore(filepath.Join(root, "volumes"))
	require.NoError(t, err)
	v, err := store.Create(name, 1<<20)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(v.DiskPath, []byte(content), 0o600))
	return filepath.Join(root, "images"), v.DiskPath
}

// Migrating copies a legacy volume into the daemon's store. Whatever fails,
// the source volume must still be there, byte for byte: it is the only copy
// of the user's data until the daemon has confirmed the import.
func TestLocalFault_VolumeMigrate(t *testing.T) {
	const data = "the user's database files"
	cases := map[string]struct {
		setup func(d *fdDaemon)
		want  string
	}{
		"daemon cannot list VMs": {func(d *fdDaemon) { d.on("VM.List", fdFail(-32000, "state store unavailable")) }, "state store unavailable"},
		"daemon dies on list":    {func(d *fdDaemon) { d.on("VM.List", fdHangup()) }, ""},
		"daemon refuses import": {func(d *fdDaemon) {
			d.on("VM.List", fdOK([]any{})).on("Volume.Import", fdFail(-32000, "volume data already exists"))
		}, "already exists"},
		"daemon dies during import": {func(d *fdDaemon) { d.on("VM.List", fdOK([]any{})).on("Volume.Import", fdHangup()) }, ""},
		"import answer cut short": {func(d *fdDaemon) {
			d.on("VM.List", fdOK([]any{})).on("Volume.Import", func(int, api.Request) fdReply {
				return fdReply{drain: true, raw: []byte(`{"jsonrpc":"2.0","id":1,"result":{"id":"da`)}
			})
		}, ""},
		"daemon too old to import": {func(d *fdDaemon) { d.on("VM.List", fdOK([]any{})) }, "method not found"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			storeFlag, disk := lfLegacyVolume(t, "data", data)
			d := fdNew(t)
			c.setup(d)
			fdIsolate(t)
			res := fdExec(t, "", "--host", d.endpoint, "--store", storeFlag, "volume", "migrate", "data")
			fdRequireCleanFailure(t, res, c.want)
			assert.Empty(t, res.stdout, "a volume was reported as migrated")
			got, err := os.ReadFile(disk)
			require.NoError(t, err, "the source volume is gone after a failed migration")
			assert.Equal(t, data, string(got))
		})
	}

	t.Run("volume in use by a VM is not copied", func(t *testing.T) {
		storeFlag, disk := lfLegacyVolume(t, "data", data)
		d := fdNew(t).
			on("VM.List", fdOK([]api.VMInfo{{ID: "vm-1", State: "running"}})).
			on("VM.Inspect", fdOK(api.VMDetail{ID: "vm-1", Volumes: []api.VolumeMountSpec{{Name: "data", DiskPath: hostPathForDaemon(disk)}}})).
			on("Volume.Import", fdOK(map[string]string{"id": "data"}))
		fdIsolate(t)
		res := fdExec(t, "", "--host", d.endpoint, "--store", storeFlag, "volume", "migrate", "data")
		fdRequireCleanFailure(t, res, "referenced by VM vm-1")
		assert.Zero(t, d.count("Volume.Import"), "a volume a VM is writing to was copied")
	})

	t.Run("a VM that cannot be inspected stops the migration", func(t *testing.T) {
		storeFlag, _ := lfLegacyVolume(t, "data", data)
		d := fdNew(t).
			on("VM.List", fdOK([]api.VMInfo{{ID: "vm-1", State: "running"}})).
			on("VM.Inspect", fdFail(-32000, "vm vm-1: state unreadable")).
			on("Volume.Import", fdOK(map[string]string{"id": "data"}))
		fdIsolate(t)
		res := fdExec(t, "", "--host", d.endpoint, "--store", storeFlag, "volume", "migrate", "data")
		fdRequireCleanFailure(t, res, "state unreadable")
		assert.Zero(t, d.count("Volume.Import"), "the volume was copied without knowing whether a VM uses it")
	})

	t.Run("success sends the whole disk and keeps the source", func(t *testing.T) {
		storeFlag, disk := lfLegacyVolume(t, "data", data)
		d := fdNew(t).on("VM.List", fdOK([]any{})).on("Volume.Import", func(int, api.Request) fdReply {
			return fdReply{drain: true, result: map[string]any{"id": "data", "size_bytes": len(data)}}
		})
		fdIsolate(t)
		res := fdExec(t, "", "--host", d.endpoint, "--store", storeFlag, "volume", "migrate", "data")
		require.NoError(t, res.err)
		assert.Equal(t, "data\n", res.stdout)
		var p api.VolumeImportParams
		d.params("Volume.Import", 0, &p)
		assert.Equal(t, "data", p.Name)
		assert.EqualValues(t, len(data), p.SizeBytes)
		assert.Contains(t, d.wire(), data, "the disk's bytes did not reach the daemon")
		got, err := os.ReadFile(disk)
		require.NoError(t, err)
		assert.Equal(t, data, string(got), "the source must be preserved")
	})
}
