//go:build linux

package vm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemoryStore_Resolve_ByID(t *testing.T) {
	s := NewMemoryStore()
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	got, err := s.Resolve(v.ID)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
}

func TestMemoryStore_Resolve_ByName(t *testing.T) {
	s := NewMemoryStore()
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M", Name: "myvm"})
	require.NoError(t, err)
	got, err := s.Resolve("myvm")
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
}

func TestMemoryStore_Resolve_ByPrefix(t *testing.T) {
	s := NewMemoryStore()
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	prefix := v.ID[:4]
	got, err := s.Resolve(prefix)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
}

func TestMemoryStore_Resolve_AmbiguousPrefix(t *testing.T) {
	s := NewMemoryStore()
	s.mu.Lock()
	s.vms["abc12345"] = &VM{ID: "abc12345", Cfg: Config{Name: "vm1"}, done: make(chan struct{})}
	s.vms["abc67890"] = &VM{ID: "abc67890", Cfg: Config{Name: "vm2"}, done: make(chan struct{})}
	s.mu.Unlock()
	_, err := s.Resolve("abc")
	require.Error(t, err)
	require.Contains(t, err.Error(), "ambiguous")
}

func TestMemoryStore_Resolve_NotFound(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.Resolve("nonexistent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestMemoryStore_Restore_NoOp(t *testing.T) {
	s := NewMemoryStore()
	require.NoError(t, s.Restore())
}

func TestMemoryStore_Save_NoOp(t *testing.T) {
	s := NewMemoryStore()
	v, err := s.Create(Config{ImagePath: "test.img", Memory: "256M"})
	require.NoError(t, err)
	require.NoError(t, s.Save(v))
}

func TestNewStore_Deprecated(t *testing.T) {
	s := NewStore()
	require.NotNil(t, s)
	require.Empty(t, s.List())
}

func TestNewID_Format(t *testing.T) {
	id, err := newID()
	require.NoError(t, err)
	require.Len(t, id, 12)
	for _, c := range id {
		require.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'))
	}
}
