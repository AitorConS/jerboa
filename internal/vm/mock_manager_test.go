//go:build linux

package vm

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMockManager_CreateAndGet(t *testing.T) {
	m := NewMockManager()
	v, err := m.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.Equal(t, StateCreated, v.GetState())

	got, err := m.Get(v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
}

func TestMockManager_StartAndStop(t *testing.T) {
	m := NewMockManager()
	v, err := m.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)

	require.NoError(t, m.Start(context.Background(), v.ID))
	require.Equal(t, StateRunning, v.GetState())

	require.NoError(t, m.Stop(context.Background(), v.ID))
	require.Equal(t, StateStopped, v.GetState())
}

func TestMockManager_List(t *testing.T) {
	m := NewMockManager()
	_, _ = m.Create(context.Background(), Config{ImagePath: "a.img", Memory: "256M"})
	_, _ = m.Create(context.Background(), Config{ImagePath: "b.img", Memory: "256M"})
	list := m.List()
	require.Len(t, list, 2)
}

func TestMockManager_Remove(t *testing.T) {
	m := NewMockManager()
	v, _ := m.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	_ = m.Start(context.Background(), v.ID)
	_ = m.Stop(context.Background(), v.ID)

	require.NoError(t, m.Remove(context.Background(), v.ID))
	_, err := m.Get(v.ID)
	require.Error(t, err)
}

func TestMockManager_CustomCreateFn(t *testing.T) {
	m := NewMockManager()
	m.CreateFn = func(_ context.Context, _ Config) (*VM, error) {
		return nil, fmt.Errorf("custom error")
	}
	_, err := m.Create(context.Background(), Config{ImagePath: "test.img", Memory: "256M"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "custom error")
}
