package vm

import (
	"context"
	"encoding/json"
	"time"
)

// fcCapabilities is what a VMM build says it can run, from
// `firecracker --capabilities`.
type fcCapabilities struct {
	MaxVCPUs    int    `json:"max_vcpus"`
	MemoryMiB   [2]int `json:"memory_mib"`
	MaxDrives   int    `json:"max_drives"`
	PeerControl bool   `json:"peer_control"`
	// SlirpMTUMax is the largest MTU a slirp link takes; 0 for a VMM whose
	// slirp link is fixed at 1500.
	SlirpMTUMax int `json:"slirp_mtu_max"`
	// PublicEgress reports that the VMM takes "public_egress" in its security
	// object; without it a slirp guest has no outbound network by default.
	PublicEgress bool `json:"public_egress"`
	// declared is false for a VMM too old to say: the bounds are then those
	// every such build had, and whether it takes a direct VM-to-VM link is
	// found by asking it about a config (preflightFCConfig).
	declared bool
}

var legacyFCCapabilities = fcCapabilities{MaxVCPUs: 4, MemoryMiB: [2]int{64, 2048}, MaxDrives: 4}

// fcCapabilities asks the VMM once in the daemon's life: a VMM replaced on
// disk is picked up when the daemon restarts.
func (m *FirecrackerManager) fcCapabilities() fcCapabilities {
	m.fcCapsOnce.Do(func() {
		c := m.askFCCapabilities()
		if c.declared && !c.PeerControl {
			m.noPeerControl.Store(true)
		}
		m.fcCaps = c
	})
	return m.fcCaps.(fcCapabilities) //nolint:forcetypeassert // only set above
}

func (m *FirecrackerManager) askFCCapabilities() fcCapabilities {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := m.mkCmd(ctx, m.fcBin, "--capabilities").Output()
	if err != nil {
		return legacyFCCapabilities
	}
	var c fcCapabilities
	if json.Unmarshal(out, &c) != nil ||
		c.MaxVCPUs < 1 || c.MaxVCPUs > 1024 ||
		c.MemoryMiB[0] < 1 || c.MemoryMiB[1] < c.MemoryMiB[0] ||
		c.MaxDrives < 1 || c.MaxDrives > 64 {
		return legacyFCCapabilities
	}
	c.declared = true
	return c
}
