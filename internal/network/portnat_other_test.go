//go:build !linux

package network

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Without Linux there is no kernel NAT. A daemon asked for it must say so at
// start instead of running with published ports that silently do not exist,
// and "auto" must settle on the proxy.
func TestInitPortPublishing_WithoutKernelNAT(t *testing.T) {
	t.Cleanup(ShutdownPortPublishing)
	cleared := 0
	prev := clearStaleNAT
	clearStaleNAT = func() { cleared++ }
	t.Cleanup(func() { clearStaleNAT = prev })

	for _, mode := range []string{PublishNAT, PublishNATLAN} {
		got, err := InitPortPublishing(mode)
		require.ErrorContains(t, err, "kernel NAT unavailable", mode)
		require.Empty(t, got)
		require.Nil(t, currentNAT())
	}
	for _, mode := range []string{"", PublishAuto} {
		got, err := InitPortPublishing(mode)
		require.NoError(t, err, "mode %q", mode)
		require.Equal(t, PublishProxy, got)
		require.Nil(t, currentNAT())
	}
	require.Zero(t, cleared, "only an explicit proxy mode clears what a previous daemon left")

	for _, mode := range []string{"NAT", "Proxy", " auto", "nat,lan", "nat-lan ", "iptables", "\x00"} {
		_, err := InitPortPublishing(mode)
		require.ErrorContains(t, err, "invalid", "mode %q", mode)
	}
}

func TestInitPortPublishing_ReplacesWhatWasActive(t *testing.T) {
	prev := clearStaleNAT
	clearStaleNAT = func() {}
	t.Cleanup(func() { clearStaleNAT = prev })

	// A translation layer left active (a daemon re-initializing) is shut
	// down exactly once whichever mode comes next, also when the new mode
	// then fails; an invalid mode touches nothing.
	for _, tc := range []struct {
		mode      string
		shutdowns int
	}{
		{PublishProxy, 1}, {PublishAuto, 1}, {PublishNAT, 1}, {PublishNATLAN, 1}, {"bogus", 0},
	} {
		n := &fakeNAT{}
		withNAT(t, n)
		_, _ = InitPortPublishing(tc.mode)
		require.Equal(t, tc.shutdowns, n.shutdowns, "mode %q", tc.mode)
		if tc.shutdowns > 0 {
			require.Nil(t, currentNAT(), "mode %q", tc.mode)
		}
	}

	n := &fakeNAT{}
	withNAT(t, n)
	ShutdownPortPublishing()
	ShutdownPortPublishing()
	require.Equal(t, 1, n.shutdowns, "shutting down twice removes the rules once")
}
