//go:build linux || (darwin && arm64)

package apiserver

import (
	"errors"
	"testing"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/stretchr/testify/require"
)

// jerboa doctor reports the guest network guard of a Linux daemon: an error
// while VMs on networks cannot start because of it, a warning when guests
// run without it, and nothing at all where no guard exists (macOS).
func TestCheckGuardFindings(t *testing.T) {
	notLoaded := errors.New("guard: nft not found")
	for name, c := range map[string]struct {
		state    network.GuardState
		severity string
		hint     string
	}{
		"loaded":        {network.GuardState{Configured: true, Enforced: true}, api.SeverityOK, ""},
		"missing":       {network.GuardState{Configured: true, Enforced: true, Err: notLoaded}, api.SeverityError, "nftables"},
		"opted out":     {network.GuardState{Configured: true, AllowUnguarded: true, Err: notLoaded}, api.SeverityWarning, network.AllowUnguardedEnv},
		"wsl, missing":  {network.GuardState{Configured: true, NoBridgeFamily: true, Err: notLoaded}, api.SeverityWarning, "nftables"},
		"wsl, loaded":   {network.GuardState{Configured: true, NoBridgeFamily: true}, api.SeverityInfo, ""},
		"no guard here": {network.GuardState{}, "", ""},
	} {
		ch := &checker{}
		checkGuard(ch, func() network.GuardState { return c.state })
		if c.severity == "" {
			require.Empty(t, ch.findings, name)
			continue
		}
		require.Len(t, ch.findings, 1, name)
		f := ch.findings[0]
		require.Equal(t, "network isolation", f.Check, name)
		require.Equal(t, c.severity, f.Severity, name)
		require.Contains(t, f.Hint, c.hint, name)
		if c.state.Err != nil {
			require.Contains(t, f.Message, notLoaded.Error(), name)
		}
	}
}
