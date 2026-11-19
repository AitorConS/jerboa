//go:build linux

package vm

import (
	"os"
	"testing"

	"github.com/AitorConS/jerboa/internal/network"
	"github.com/stretchr/testify/require"
)

// A guest is never put on a bridge the guest network guard does not
// protect: with no guard loaded (this process never installs one) the setup
// fails, says how to fix it, and creates nothing on the host first.
func TestSetupTAPNetworkRefusesAnUnguardedBridge(t *testing.T) {
	const tap = "jt-noguard0"
	cfg := Config{NetworkName: "noguard", TapName: tap, GatewayIP: "10.250.7.1", IPAddress: "10.250.7.2", SubnetMask: "24"}
	err := setupTAPNetwork(cfg)
	require.ErrorContains(t, err, "nftables")
	require.ErrorContains(t, err, network.AllowUnguardedEnv+"=1")
	_, statErr := os.Stat("/sys/class/net/" + tap)
	require.True(t, os.IsNotExist(statErr), "a tap was created for a guest that cannot be guarded")
}
