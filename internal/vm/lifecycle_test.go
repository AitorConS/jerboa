//go:build linux

package vm

import (
	"context"
	"errors"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type mockProcess struct {
	killErr   error
	signalErr error
	killed    bool
	signaled  bool
}

func (m *mockProcess) kill() error {
	m.killed = true
	if m.killErr != nil {
		return m.killErr
	}
	return nil
}

func (m *mockProcess) signal(sig os.Signal) error {
	m.signaled = true
	if m.signalErr != nil {
		return m.signalErr
	}
	return nil
}

func TestQEMUManager_Kill(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	require.Equal(t, StateRunning, v.GetState())
	require.NoError(t, mgr.Kill(context.Background(), v.ID))
	select {
	case <-v.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("VM did not stop after kill")
	}
	require.Equal(t, StateStopped, v.GetState())
	require.True(t, v.IsExplicitStop())
}

func TestQEMUManager_Kill_NotFound(t *testing.T) {
	mgr := fakeManager(false)
	err := mgr.Kill(context.Background(), "nonexistent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "nonexistent")
}

func TestQEMUManager_Kill_WrongState(t *testing.T) {
	mgr := fakeManager(false)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	err = mgr.Kill(context.Background(), v.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid transition")
}

func TestQEMUManager_Kill_NoProcess(t *testing.T) {
	mgr := fakeManager(false)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, v.transition(StateStarting))
	require.NoError(t, v.transition(StateRunning))
	v.mu.Lock()
	v.proc = nil
	v.mu.Unlock()
	require.NoError(t, mgr.Kill(context.Background(), v.ID))
	require.Equal(t, StateStopping, v.GetState())
	require.NoError(t, v.transition(StateStopped))
}

func TestQEMUManager_Signal(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	if runtime.GOOS != "windows" {
		err = mgr.Signal(context.Background(), v.ID, syscall.Signal(10))
		require.NoError(t, err)
	}
	require.NoError(t, mgr.Stop(context.Background(), v.ID))
	<-v.Done()
}

func TestQEMUManager_Signal_NotFound(t *testing.T) {
	mgr := fakeManager(false)
	err := mgr.Signal(context.Background(), "nonexistent", syscall.SIGTERM)
	require.Error(t, err)
}

func TestQEMUManager_Signal_NoProcess(t *testing.T) {
	mgr := fakeManager(false)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	err = mgr.Signal(context.Background(), v.ID, syscall.SIGTERM)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no process")
}

func TestQEMUManager_Stop_NotFound(t *testing.T) {
	mgr := fakeManager(false)
	err := mgr.Stop(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestQEMUManager_Stop_WrongState(t *testing.T) {
	mgr := fakeManager(false)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	err = mgr.Stop(context.Background(), v.ID)
	require.Error(t, err)
}

func TestQEMUManager_Start_AlreadyRunning(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	err = mgr.Start(context.Background(), v.ID)
	require.Error(t, err)
	require.NoError(t, mgr.Stop(context.Background(), v.ID))
	<-v.Done()
}

func TestQEMUManager_Start_NotFound(t *testing.T) {
	mgr := fakeManager(false)
	err := mgr.Start(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestQEMUManager_Remove_NotStopped(t *testing.T) {
	mgr := fakeManager(true)
	v, err := mgr.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(context.Background(), v.ID))
	err = mgr.Remove(context.Background(), v.ID)
	require.Error(t, err)
	require.NoError(t, mgr.Stop(context.Background(), v.ID))
	<-v.Done()
}

func TestMockManager_Kill(t *testing.T) {
	m := NewMockManager()
	v, err := m.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, m.Start(context.Background(), v.ID))
	require.NoError(t, m.Kill(context.Background(), v.ID))
	require.Equal(t, StateStopped, v.GetState())
}

func TestMockManager_Kill_NotFound(t *testing.T) {
	m := NewMockManager()
	err := m.Kill(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestMockManager_Signal(t *testing.T) {
	m := NewMockManager()
	err := m.Signal(context.Background(), "any", syscall.SIGTERM)
	require.NoError(t, err)
}

func TestMockManager_Signal_CustomFn(t *testing.T) {
	m := NewMockManager()
	m.SignalFn = func(_ context.Context, _ string, _ os.Signal) error {
		return errors.New("signal failed")
	}
	err := m.Signal(context.Background(), "vm1", syscall.SIGTERM)
	require.Error(t, err)
	require.Contains(t, err.Error(), "signal failed")
}

func TestMockManager_Kill_CustomFn(t *testing.T) {
	m := NewMockManager()
	m.KillFn = func(_ context.Context, _ string) error {
		return errors.New("kill failed")
	}
	err := m.Kill(context.Background(), "vm1")
	require.Error(t, err)
}

func TestMockManager_Stop_NotFound(t *testing.T) {
	m := NewMockManager()
	err := m.Stop(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestMockManager_Start_NotFound(t *testing.T) {
	m := NewMockManager()
	err := m.Start(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestMockManager_Remove_NotStopped(t *testing.T) {
	m := NewMockManager()
	v, _ := m.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	_ = m.Start(context.Background(), v.ID)
	err := m.Remove(context.Background(), v.ID)
	require.Error(t, err)
}

func TestMockManager_Remove_NotFound(t *testing.T) {
	m := NewMockManager()
	err := m.Remove(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestMockManager_Get_NotFound(t *testing.T) {
	m := NewMockManager()
	_, err := m.Get("nonexistent")
	require.Error(t, err)
}

func TestMockManager_Stop_CustomFn(t *testing.T) {
	m := NewMockManager()
	m.StopFn = func(_ context.Context, _ string) error {
		return errors.New("stop failed")
	}
	err := m.Stop(context.Background(), "vm1")
	require.Error(t, err)
}

func TestMockManager_Start_CustomFn(t *testing.T) {
	m := NewMockManager()
	m.StartFn = func(_ context.Context, _ string) error {
		return errors.New("start failed")
	}
	err := m.Start(context.Background(), "vm1")
	require.Error(t, err)
}

func TestMockManager_Remove_CustomFn(t *testing.T) {
	m := NewMockManager()
	m.RemoveFn = func(_ context.Context, _ string) error {
		return errors.New("remove failed")
	}
	err := m.Remove(context.Background(), "vm1")
	require.Error(t, err)
}

func TestMockManager_Get_CustomFn(t *testing.T) {
	m := NewMockManager()
	m.GetFn = func(_ string) (*VM, error) {
		return nil, errors.New("get failed")
	}
	_, err := m.Get("vm1")
	require.Error(t, err)
}

func TestMockManager_List_CustomFn(t *testing.T) {
	m := NewMockManager()
	m.ListFn = func() []*VM { return nil }
	require.Nil(t, m.List())
}

func TestVM_Logs(t *testing.T) {
	v := &VM{done: make(chan struct{})}
	v.logBuf.Write([]byte("hello world"))
	require.Equal(t, []byte("hello world"), v.Logs())
}

func TestVM_AttachReader_Nil(t *testing.T) {
	v := &VM{done: make(chan struct{})}
	require.Nil(t, v.AttachReader())
}

func TestVM_GetTimes(t *testing.T) {
	now := time.Now()
	v := &VM{done: make(chan struct{}), StartedAt: &now}
	startedAt, stoppedAt := v.GetTimes()
	require.NotNil(t, startedAt)
	require.Nil(t, stoppedAt)
	require.Equal(t, now, *startedAt)
}

func TestVM_GetTimes_BothNil(t *testing.T) {
	v := &VM{done: make(chan struct{})}
	startedAt, stoppedAt := v.GetTimes()
	require.Nil(t, startedAt)
	require.Nil(t, stoppedAt)
}

func TestSafeBuffer_Write(t *testing.T) {
	var buf safeBuffer
	n, err := buf.Write([]byte("abc"))
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.Equal(t, []byte("abc"), buf.Bytes())
}

func TestSafeBuffer_Write_Multiple(t *testing.T) {
	var buf safeBuffer
	buf.Write([]byte("hello "))
	buf.Write([]byte("world"))
	require.Equal(t, []byte("hello world"), buf.Bytes())
}

func TestSafeBuffer_Bytes_Copy(t *testing.T) {
	var buf safeBuffer
	buf.Write([]byte("data"))
	b := buf.Bytes()
	b[0] = 'X'
	require.Equal(t, []byte("data"), buf.Bytes(), "Bytes() should return a copy")
}

func TestOsProcess_Kill_NilProcess(t *testing.T) {
	o := &osProcess{p: nil}
	require.Panics(t, func() { _ = o.kill() })
}

func TestOsProcess_Signal_NilProcess(t *testing.T) {
	o := &osProcess{p: nil}
	require.Panics(t, func() { _ = o.signal(syscall.SIGTERM) })
}
