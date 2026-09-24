//go:build linux || (darwin && arm64)

package network

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecoverInterruptedRelease(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	_, err = s.Create("bench", "10.100.0.0/24", "bridge")
	require.NoError(t, err)
	for _, ip := range []string{"10.100.0.2", "10.100.0.3", "10.100.0.4"} {
		require.NoError(t, s.ReserveIP("bench", ip))
	}
	require.NoError(t, s.PrepareRelease("bench", "deleted-vm", "10.100.0.2"))
	require.NoError(t, s.PrepareRelease("bench", "live-vm", "10.100.0.3"))
	s, err = NewStore(root)
	require.NoError(t, err)
	require.NoError(t, s.RecoverReleases(map[string]bool{"live-vm": true}))
	require.NoError(t, s.ReserveIP("bench", "10.100.0.2"))
	require.ErrorIs(t, s.ReserveIP("bench", "10.100.0.3"), ErrIPAlreadyAllocated)
	require.ErrorIs(t, s.ReserveIP("bench", "10.100.0.4"), ErrIPAlreadyAllocated)
	// Recovery is idempotent and must not reclaim the reused address.
	require.NoError(t, s.RecoverReleases(nil))
	require.ErrorIs(t, s.ReserveIP("bench", "10.100.0.2"), ErrIPAlreadyAllocated)
}
