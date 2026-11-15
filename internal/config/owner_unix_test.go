//go:build unix

package config

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const plantedConfig = "[daemon]\nendpoint = \"tcp://198.51.100.7:7890\"\ntoken = \"planted\"\n"

// noHomeFallback points DefaultPath at the shared temp fallback with TMPDIR set
// to a fresh directory, and returns the fallback's jerboa-<uid> directory and
// config path.
func noHomeFallback(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("HOME", "")
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("JERBOA_HOST", "")
	t.Setenv("JERBOA_AUTH_TOKEN", "")
	path := DefaultPath()
	require.Equal(t, filepath.Join(noHomeDir(), "config.toml"), path)
	return filepath.Dir(filepath.Dir(path)), path
}

func TestLoad_NoHomeFallbackReadsPrivateFile(t *testing.T) {
	_, path := noHomeFallback(t)
	require.NoError(t, Save(path, &Config{Daemon: DaemonConfig{Endpoint: "tcp://127.0.0.1:7890"}}))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "tcp://127.0.0.1:7890", cfg.Daemon.Endpoint)
}

func TestLoad_NoHomeFallbackMissingIsDefaults(t *testing.T) {
	_, path := noHomeFallback(t)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, &Config{}, cfg)
}

func TestLoad_NoHomeFallbackRefusesWritableFile(t *testing.T) {
	_, path := noHomeFallback(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(plantedConfig), 0o600))
	require.NoError(t, os.Chmod(path, 0o666))
	_, err := Load(path)
	require.Error(t, err)
	assert.Equal(t, DefaultEndpoint(), ResolveEndpoint(""))
	assert.Empty(t, ResolveToken())
}

func TestLoad_NoHomeFallbackRefusesSymlinkedFile(t *testing.T) {
	_, path := noHomeFallback(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	require.NoError(t, os.WriteFile(target, []byte(plantedConfig), 0o600))
	require.NoError(t, os.Symlink(target, path))
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoad_NoHomeFallbackRefusesSymlinkedParent(t *testing.T) {
	userDir, path := noHomeFallback(t)
	elsewhere := t.TempDir()
	require.NoError(t, os.Chmod(elsewhere, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(elsewhere, "config.toml"), []byte(plantedConfig), 0o600))
	require.NoError(t, os.Mkdir(userDir, 0o700))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(userDir, ".jerboa")))
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a private directory")
}

func TestLoad_NoHomeFallbackRefusesWritableParent(t *testing.T) {
	userDir, path := noHomeFallback(t)
	require.NoError(t, Save(path, &Config{Daemon: DaemonConfig{Endpoint: "tcp://198.51.100.7:7890"}}))
	// Someone who can write jerboa-<uid> can rename .jerboa away and put
	// their own in its place.
	require.NoError(t, os.Chmod(userDir, 0o777))
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a private directory")

	require.NoError(t, os.Chmod(userDir, 0o700))
	require.NoError(t, os.Chmod(filepath.Dir(path), 0o770))
	_, err = Load(path)
	require.Error(t, err)
}

// TestLoad_NoHomeFallbackRaceNeverReturnsWritableFile toggles a mode-0666
// file in and out of the path while Load runs: the check and the read are on
// the same descriptor, so the file must never be returned.
func TestLoad_NoHomeFallbackRaceNeverReturnsWritableFile(t *testing.T) {
	_, path := noHomeFallback(t)
	dir := filepath.Dir(path)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	staged := filepath.Join(dir, "staged")
	require.NoError(t, os.WriteFile(staged, []byte(plantedConfig), 0o600))
	require.NoError(t, os.Chmod(staged, 0o666))

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = os.Rename(staged, path)
			_ = os.Rename(path, staged)
		}
	}()
	for range 500 {
		cfg, err := Load(path)
		if err == nil {
			require.Empty(t, cfg.Daemon.Endpoint, "a non-private file was read")
		}
	}
	stop.Store(true)
	wg.Wait()
}

// TestLoad_TempFileOutsideFallbackRefusesSymlink keeps the leaf check for
// other paths under the temp directory.
func TestLoad_TempFileOutsideFallbackRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.toml")
	require.NoError(t, os.WriteFile(target, []byte(plantedConfig), 0o600))
	link := filepath.Join(dir, "config.toml")
	require.NoError(t, os.Symlink(target, link))
	_, err := Load(link)
	require.Error(t, err)

	cfg, err := Load(target)
	require.NoError(t, err)
	assert.Equal(t, "tcp://198.51.100.7:7890", cfg.Daemon.Endpoint)
}

var secretConfig = &Config{Daemon: DaemonConfig{Token: "victim-secret"}}

func TestSave_NoHomeFallbackCreatesPrivateDirs(t *testing.T) {
	userDir, path := noHomeFallback(t)
	require.NoError(t, Save(path, secretConfig))
	for _, dir := range []string{userDir, filepath.Dir(path)} {
		fi, err := os.Lstat(dir)
		require.NoError(t, err)
		assert.True(t, fi.IsDir())
		assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm(), dir)
	}
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "victim-secret", cfg.Daemon.Token)

	// Saving again replaces the file in place.
	require.NoError(t, Save(path, &Config{Hypervisor: "qemu"}))
	cfg, err = Load(path)
	require.NoError(t, err)
	assert.Equal(t, "qemu", cfg.Hypervisor)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left behind")
}

func TestSave_NoHomeFallbackRefusesSymlinkedDirs(t *testing.T) {
	for _, which := range []string{"jerboa-uid", ".jerboa"} {
		t.Run(which, func(t *testing.T) {
			userDir, path := noHomeFallback(t)
			elsewhere := t.TempDir()
			link := userDir
			if which == ".jerboa" {
				require.NoError(t, os.Mkdir(userDir, 0o700))
				link = filepath.Dir(path)
			}
			require.NoError(t, os.Symlink(elsewhere, link))
			err := Save(path, secretConfig)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not a private directory")
			entries, err := os.ReadDir(elsewhere)
			require.NoError(t, err)
			assert.Empty(t, entries, "nothing is written through the symlink")
		})
	}
}

func TestSave_NoHomeFallbackRefusesFileInPlaceOfDir(t *testing.T) {
	userDir, path := noHomeFallback(t)
	require.NoError(t, os.WriteFile(userDir, nil, 0o600))
	require.Error(t, Save(path, secretConfig))
}

func TestSave_NoHomeFallbackRefusesForeignOwnedDir(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to make a directory another user owns")
	}
	userDir, path := noHomeFallback(t)
	require.NoError(t, os.Mkdir(userDir, 0o777))
	require.NoError(t, os.Chown(userDir, 65534, 65534))
	err := Save(path, secretConfig)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a private directory")
	_, err = os.Lstat(filepath.Dir(path))
	require.True(t, os.IsNotExist(err), "nothing is created inside it")
}

func TestSave_NoHomeFallbackNarrowsOwnWritableDir(t *testing.T) {
	userDir, path := noHomeFallback(t)
	require.NoError(t, os.Mkdir(userDir, 0o700))
	require.NoError(t, os.Chmod(userDir, 0o777))
	require.NoError(t, Save(path, secretConfig))
	fi, err := os.Lstat(userDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	_, err = Load(path)
	require.NoError(t, err)
}

func TestSave_NoHomeFallbackReplacesPlantedSymlink(t *testing.T) {
	_, path := noHomeFallback(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	target := filepath.Join(t.TempDir(), "victim-target")
	require.NoError(t, os.WriteFile(target, []byte("untouched"), 0o600))
	require.NoError(t, os.Symlink(target, path))
	require.NoError(t, Save(path, secretConfig))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(got), "the symlink is replaced, not followed")
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular())
}

func TestWritePrivateFile_DaemonFileFallback(t *testing.T) {
	userDir, _ := noHomeFallback(t)
	path := DaemonFilePath()
	require.Equal(t, filepath.Join(noHomeDir(), "daemon.json"), path)
	elsewhere := t.TempDir()
	require.NoError(t, os.Symlink(elsewhere, userDir))
	require.Error(t, WritePrivateFile(path, []byte(`{"token":"x"}`)))
	_, err := ReadPrivateFile(path)
	require.Error(t, err)
}
