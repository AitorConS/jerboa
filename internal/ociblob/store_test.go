package ociblob

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStore_PutOpenExistsDelete(t *testing.T) {
	t.Parallel()

	s, err := NewStore(t.TempDir())
	require.NoError(t, err)

	digest, size, err := s.Put(bytes.NewBufferString("hello"))
	require.NoError(t, err)
	require.Equal(t, int64(5), size)
	require.True(t, s.Exists(digest))

	rc, err := s.Open(digest)
	require.NoError(t, err)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))
	require.NoError(t, rc.Close())

	require.NoError(t, s.Delete(digest))
	require.False(t, s.Exists(digest))
}

func TestStore_PutDeduplicates(t *testing.T) {
	t.Parallel()

	s, err := NewStore(t.TempDir())
	require.NoError(t, err)

	d1, _, err := s.Put(bytes.NewBufferString("same"))
	require.NoError(t, err)
	d2, _, err := s.Put(bytes.NewBufferString("same"))
	require.NoError(t, err)
	require.Equal(t, d1, d2)

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestStore_OpenNotFound(t *testing.T) {
	t.Parallel()

	s, err := NewStore(t.TempDir())
	require.NoError(t, err)

	_, err = s.Open("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}
