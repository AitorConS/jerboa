//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// End to end over the real socket and wire protocol: CLI -> daemon -> stores.

// leaveUntagged stores a second disk and drops its name, as a rebuild does.
func leaveUntagged(t *testing.T, storePath string) {
	t.Helper()
	store, err := image.NewStore(storePath)
	require.NoError(t, err)
	disk := filepath.Join(t.TempDir(), "old.img")
	require.NoError(t, os.WriteFile(disk, []byte("an older build of the image"), 0o600))
	m := image.Manifest{SchemaVersion: 1, Name: "old", Tag: "x", Created: time.Now(), Config: image.Config{Memory: "256M", CPUs: 1}}
	require.NoError(t, store.Put("old", "x", m, disk))
	refs := map[string]string{}
	data, err := os.ReadFile(filepath.Join(storePath, "refs.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &refs))
	delete(refs, "old:x")
	data, err = json.Marshal(refs)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(storePath, "refs.json"), data, 0o644))
}

func TestSystemDF_EndToEnd(t *testing.T) {
	storePath, _ := makeStore(t)
	_, socketPath := startDaemonWithStore(t, storePath)

	out := execRoot(t, socketPath, storePath, "system", "df")
	assert.Contains(t, out, "TYPE")
	assert.Contains(t, out, "Images")
	assert.Contains(t, out, "Volumes")
	assert.Contains(t, out, "can be reclaimed with `jerboa system prune`")

	var df api.SystemDF
	require.NoError(t, json.Unmarshal([]byte(execRoot(t, socketPath, storePath, "--output", "json", "system", "df")), &df))
	var images api.SystemUsage
	for _, u := range df.Usage {
		if u.Kind == "images" {
			images = u
		}
	}
	assert.Equal(t, 1, images.Count)
	assert.Positive(t, images.Bytes)
}

func TestSystemPrune_EndToEnd(t *testing.T) {
	storePath, _ := makeStore(t)
	leaveUntagged(t, storePath)
	_, socketPath := startDaemonWithStore(t, storePath)

	dry := execRoot(t, socketPath, storePath, "system", "prune", "--dry-run")
	assert.Contains(t, dry, "Would remove:")
	assert.Contains(t, dry, "<untagged>")
	assert.NotContains(t, dry, "hello:latest", "a named image is not offered without --all")

	// Without -y and with no terminal to answer, nothing is removed.
	msg := execRootExpectError(t, socketPath, storePath, "system", "prune")
	assert.Contains(t, msg, "canceled")
	assert.Contains(t, execRoot(t, socketPath, storePath, "system", "prune", "--dry-run"), "<untagged>")

	out := execRoot(t, socketPath, storePath, "system", "prune", "-y")
	assert.Contains(t, out, "Removed:")
	assert.Contains(t, out, "Reclaimed:")
	assert.Equal(t, "Nothing to prune.\n", execRoot(t, socketPath, storePath, "system", "prune", "-y"))
	assert.Contains(t, execRoot(t, socketPath, storePath, "images"), "hello", "the named image survived the prune")

	var res api.SystemPruneResult
	require.NoError(t, json.Unmarshal([]byte(execRoot(t, socketPath, storePath, "--output", "json", "system", "prune", "--all", "--dry-run")), &res))
	require.Len(t, res.Removed, 1)
	assert.Equal(t, "hello:latest", res.Removed[0].Name)
	assert.True(t, res.DryRun)

	execRoot(t, socketPath, storePath, "system", "prune", "--all", "-y")
	assert.NotContains(t, execRoot(t, socketPath, storePath, "images"), "hello")
}

func TestDoctor_EndToEnd(t *testing.T) {
	storePath, _ := makeStore(t)
	_, socketPath := startDaemonWithStore(t, storePath)

	out := execRoot(t, socketPath, storePath, "doctor")
	assert.Contains(t, out, "[ ok ] cli")
	assert.Contains(t, out, "[ ok ] daemon")
	assert.Contains(t, out, "[ ok ] images")
	assert.Contains(t, out, "1 image readable")
	assert.NotContains(t, out, "[FAIL]")

	out = execRoot(t, socketPath, storePath, "doctor", "--deep")
	assert.Contains(t, out, "1 image verified against their digests")
}

func TestDoctor_EndToEnd_FindsDamage(t *testing.T) {
	storePath, _ := makeStore(t)
	_, socketPath := startDaemonWithStore(t, storePath)

	// Change the stored disk behind the daemon's back.
	entries, err := os.ReadDir(storePath)
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			disk := filepath.Join(storePath, e.Name(), "disk.img")
			require.NoError(t, os.Chmod(disk, 0o644))
			require.NoError(t, os.WriteFile(disk, []byte("FAKE disk content"), 0o644))
		}
	}

	assert.NotContains(t, execRoot(t, socketPath, storePath, "doctor"), "[FAIL]", "the quick check does not read disks")
	msg := execRootExpectError(t, socketPath, storePath, "doctor", "--deep")
	assert.Contains(t, msg, "doctor: 1 problem(s) found")

	var report struct {
		OK       bool                `json:"ok"`
		Findings []api.SystemFinding `json:"findings"`
	}
	root := newRootCmd()
	out := &jsonBuffer{}
	root.SetOut(out)
	root.SetErr(&jsonBuffer{})
	root.SetArgs([]string{"--host", socketPath, "--store", storePath, "--output", "json", "doctor", "--deep"})
	require.Error(t, root.Execute())
	require.NoError(t, json.Unmarshal(out.data, &report))
	assert.False(t, report.OK)
	found := false
	for _, f := range report.Findings {
		if f.Severity == api.SeverityError {
			found = true
			assert.Equal(t, "images", f.Check)
			assert.Contains(t, f.Message, "hello:latest is corrupt")
			assert.Contains(t, f.Hint, "jerboa rmi hello:latest")
		}
	}
	assert.True(t, found)
}

type jsonBuffer struct{ data []byte }

func (b *jsonBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}
