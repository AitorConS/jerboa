//go:build linux || (darwin && arm64)

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The VM registry after things go wrong: a write that fails, a file a power
// cut emptied, a daemon that died mid-save. One damaged record must cost at
// most that VM, and never the daemon's ability to start.

func restored(t *testing.T, dir string) *FileStore {
	t.Helper()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore(), "the registry must load whatever is on disk")
	return s
}

func ids(s *FileStore) []string {
	var out []string
	for _, v := range s.List() {
		out = append(out, v.ID)
	}
	return out
}

func TestFaults_CreateVMIsAllOrNothing(t *testing.T) {
	for _, f := range durabletest.Failures() {
		if f.Step == durable.StepSync || f.Step == durable.StepSyncDir {
			continue // VM state is not flushed, so those steps never run
		}
		t.Run(f.Name, func(t *testing.T) {
			dir := t.TempDir()
			s := restored(t, dir)
			keep, err := s.Create(Config{Name: "keep", ImagePath: "a.img"})
			require.NoError(t, err)

			inj := durabletest.Inject(t, f, 0, nil)
			_, err = s.Create(Config{Name: "fresh", ImagePath: "b.img"})
			require.True(t, inj.Fired())
			require.Error(t, err)
			durable.SetFault(nil)

			assert.Equal(t, []string{keep.ID}, ids(s), "a VM that could not be persisted must not stay registered")
			again := restored(t, dir)
			assert.Equal(t, []string{keep.ID}, ids(again))
			got, err := again.Get(keep.ID)
			require.NoError(t, err)
			assert.Equal(t, "keep", got.Cfg.Name)
		})
	}
}

func TestFaults_SaveFailureKeepsThePreviousRecord(t *testing.T) {
	for _, f := range durabletest.Failures() {
		if f.Step == durable.StepSync || f.Step == durable.StepSyncDir {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			dir := t.TempDir()
			s := restored(t, dir)
			v, err := s.Create(Config{Name: "app", ImagePath: "a.img"})
			require.NoError(t, err)
			v.mu.Lock()
			v.RestartCount = 1
			v.mu.Unlock()
			require.NoError(t, s.Save(v))

			v.mu.Lock()
			v.RestartCount = 2
			v.mu.Unlock()
			inj := durabletest.Inject(t, f, 0, nil)
			require.Error(t, s.Save(v))
			require.True(t, inj.Fired())
			durable.SetFault(nil)

			got, err := restored(t, dir).Get(v.ID)
			require.NoError(t, err, "a failed save lost the VM")
			assert.Equal(t, 1, got.RestartCount, "the record on disk must be the last one that was saved whole")

			require.NoError(t, s.Save(v))
			got, err = restored(t, dir).Get(v.ID)
			require.NoError(t, err)
			assert.Equal(t, 2, got.RestartCount)
		})
	}
}

func TestFaults_UnreadableRecordCostsOnlyThatVM(t *testing.T) {
	damage := map[string]func(t *testing.T, path string){
		"deleted": func(t *testing.T, p string) { require.NoError(t, os.Remove(p)) },
		"empty":   func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, nil, 0o600)) },
		"truncated": func(t *testing.T, p string) {
			require.NoError(t, os.WriteFile(p, []byte(`{"id":"abc","config":{"na`), 0o600))
		},
		"garbage": func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, []byte{0x00, 0xff, 0x10}, 0o600)) },
		"zeroes":  func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, make([]byte, 512), 0o600)) },
		"wrong type": func(t *testing.T, p string) {
			require.NoError(t, os.WriteFile(p, []byte(`{"id":7,"config":"nope"}`), 0o600))
		},
		"is a directory": func(t *testing.T, p string) {
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Mkdir(p, 0o700))
		},
	}
	for name, fn := range damage {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := restored(t, dir)
			broken, err := s.Create(Config{Name: "broken", ImagePath: "a.img"})
			require.NoError(t, err)
			fine, err := s.Create(Config{Name: "fine", ImagePath: "b.img"})
			require.NoError(t, err)

			fn(t, filepath.Join(dir, broken.ID, "state.json"))

			again := restored(t, dir)
			assert.Equal(t, []string{fine.ID}, ids(again))
			// The registry keeps working: new VMs, and removal of the survivor.
			_, err = again.Create(Config{Name: "new", ImagePath: "c.img"})
			require.NoError(t, err)
			require.NoError(t, again.Remove(fine.ID))
		})
	}
}

func TestFaults_RestoreIgnoresWhatIsNotAVM(t *testing.T) {
	dir := t.TempDir()
	s := restored(t, dir)
	v, err := s.Create(Config{Name: "app", ImagePath: "a.img"})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "empty-dir"), 0o700))
	// A temporary file a killed daemon left beside a record.
	require.NoError(t, os.WriteFile(filepath.Join(dir, v.ID, ".state.json.tmp-123"), []byte("{half"), 0o600))

	assert.Equal(t, []string{v.ID}, ids(restored(t, dir)))
}

func TestFaults_RestoreWithoutDirectory(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "never-created"))
	require.NoError(t, s.Restore())
	assert.Empty(t, s.List())
	_, err := s.Create(Config{Name: "first", ImagePath: "a.img"})
	require.NoError(t, err, "the first VM creates the registry directory")
}

func TestFaults_RunningVMWhoseProcessDiedIsStoppedOnRestore(t *testing.T) {
	for _, state := range []State{StateRunning, StateStarting, StateStopping} {
		t.Run(string(state), func(t *testing.T) {
			dir := t.TempDir()
			s := restored(t, dir)
			v, err := s.Create(Config{Name: "app", ImagePath: "a.img"})
			require.NoError(t, err)
			v.mu.Lock()
			v.State = state
			v.pid = 0x7ffffff0 // no such process: the machine was power-cycled
			v.mu.Unlock()
			require.NoError(t, s.Save(v))

			again := restored(t, dir)
			got, err := again.Get(v.ID)
			require.NoError(t, err)
			assert.Equal(t, StateStopped, got.GetState())
			assert.True(t, got.DaemonRecovered)
			_, stoppedAt := got.GetTimes()
			assert.NotNil(t, stoppedAt)

			// The recovered state was written back, so the next start does
			// not recover it a second time.
			third := restored(t, dir)
			got, err = third.Get(v.ID)
			require.NoError(t, err)
			assert.Equal(t, StateStopped, got.GetState())
		})
	}
}

func TestFaults_RecordKeepsSecretsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	s := restored(t, dir)
	v, err := s.Create(Config{Name: "app", ImagePath: "a.img", Env: []string{"TOKEN=secret"}})
	require.NoError(t, err)
	for range 3 {
		require.NoError(t, s.Save(v))
	}
	info, err := os.Stat(filepath.Join(dir, v.ID, "state.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dinfo, err := os.Stat(filepath.Join(dir, v.ID))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dinfo.Mode().Perm())
}

func TestFaults_RemoveThenRestore(t *testing.T) {
	dir := t.TempDir()
	s := restored(t, dir)
	a, err := s.Create(Config{Name: "a", ImagePath: "a.img"})
	require.NoError(t, err)
	b, err := s.Create(Config{Name: "b", ImagePath: "b.img"})
	require.NoError(t, err)
	require.NoError(t, s.Remove(a.ID))
	// Saving a VM that was removed must not bring its record back.
	require.NoError(t, s.Save(a))

	assert.Equal(t, []string{b.ID}, ids(restored(t, dir)))
	_, err = os.Stat(filepath.Join(dir, a.ID))
	assert.True(t, os.IsNotExist(err))
}
