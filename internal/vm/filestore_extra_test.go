package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileStore_Create_WriteFails_Rollback(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())
	s.mu.Lock()
	s.root = filepath.Join(dir, "blocked")
	s.mu.Unlock()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blocked"), []byte("x"), 0o644))
	_, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.Error(t, err)
}

func TestFileStore_Remove_NotFound(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())
	err := s.Remove("nonexistent")
	require.Error(t, err)
}

func TestFileStore_Restore_BadJSON(t *testing.T) {
	dir := t.TempDir()
	badDir := filepath.Join(dir, "badvm")
	require.NoError(t, os.MkdirAll(badDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(badDir, "state.json"), []byte("{invalid}"), 0o644))
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())
	_, err := s.Get("badvm")
	require.Error(t, err)
}

func TestFileStore_Restore_FileInRoot(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("not a dir"), 0o644))
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())
	require.Empty(t, s.List())
}

func TestFileStore_Restore_MissingStateJSON(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "emptyvm"), 0o755))
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())
	require.Empty(t, s.List())
}

func TestFileStore_WriteState_MarshalError(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.Restore())
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	vmDir := filepath.Join(dir, v.ID)
	tmpFile := filepath.Join(vmDir, "state.json.tmp")
	require.NoError(t, os.WriteFile(tmpFile, []byte("lock"), 0o444))
	defer os.Remove(tmpFile)
	require.NoError(t, os.Chmod(vmDir, 0o444))
	defer os.Chmod(vmDir, 0o755)
}

func TestFileStore_Restore_StartingState(t *testing.T) {
	dir := t.TempDir()
	s1 := NewFileStore(dir)
	require.NoError(t, s1.Restore())
	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, s1.Save(v))
	s2 := NewFileStore(dir)
	require.NoError(t, s2.Restore())
	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, got.GetState())
	require.True(t, got.DaemonRecovered)
}

func TestFileStore_Save_RestartCount(t *testing.T) {
	dir := t.TempDir()
	s1 := NewFileStore(dir)
	require.NoError(t, s1.Restore())
	v, err := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	v.mu.Lock()
	v.RestartCount = 5
	v.mu.Unlock()
	require.NoError(t, s1.Save(v))
	s2 := NewFileStore(dir)
	require.NoError(t, s2.Restore())
	got, err := s2.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, 5, got.RestartCount)
}

func TestFileStore_Restore_DoneChannelOpen(t *testing.T) {
	dir := t.TempDir()
	s1 := NewFileStore(dir)
	require.NoError(t, s1.Restore())
	v, _ := s1.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, v.transition(StateRunning))
	require.NoError(t, s1.Save(v))
	s2 := NewFileStore(dir)
	require.NoError(t, s2.Restore())
	got, _ := s2.Get(v.ID)
	select {
	case <-got.Done():
	default:
		t.Fatal("done channel should be closed for stopped/recovered VMs")
	}
}

func TestVMState_RoundTrip(t *testing.T) {
	st := vmState{
		ID:           "abc123",
		Config:       Config{ImagePath: "test.img", Memory: "512M", Name: "roundtrip"},
		State:        StateCreated,
		RestartCount: 3,
	}
	data, err := json.Marshal(st)
	require.NoError(t, err)
	var decoded vmState
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, st.ID, decoded.ID)
	require.Equal(t, st.Config.Name, decoded.Config.Name)
	require.Equal(t, StateCreated, decoded.State)
	require.Equal(t, 3, decoded.RestartCount)
}
