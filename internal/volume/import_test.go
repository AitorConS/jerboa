package volume

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImportPreservesDiskAndRejectsOverwrite(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	data := make([]byte, 3<<20)
	copy(data[1<<20:], []byte("persistent data"))
	v, err := s.Import(context.Background(), "data", "data", int64(len(data)), bytes.NewReader(data))
	require.NoError(t, err)
	got, err := os.ReadFile(v.DiskPath)
	require.NoError(t, err)
	require.Equal(t, data, got)
	_, err = s.Import(context.Background(), "data", "data", 1, bytes.NewReader([]byte{1}))
	require.ErrorContains(t, err, "already exists")
	got, err = os.ReadFile(v.DiskPath)
	require.NoError(t, err)
	require.Equal(t, data, got)
}

func TestImportIncompleteStreamIsNotPublished(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	for _, count := range []int{2, 4} {
		_, err = s.Import(context.Background(), "data", "data", 3, bytes.NewReader(make([]byte, count)))
		require.Error(t, err)
		_, err = s.Get("data")
		require.Error(t, err)
		entries, err := os.ReadDir(s.root)
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}
