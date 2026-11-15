//go:build integration

package integration

import (
	"os"
	"testing"

	"github.com/AitorConS/jerboa/internal/hypervisor"
	"github.com/stretchr/testify/require"
)

// TestDefaultHypervisorOnAKVMHost checks the choice a daemon makes when no
// hypervisor was chosen, on a real host that has KVM and Firecracker: it is
// Firecracker, unless this process may not create network devices, and then
// the reason says so.
func TestDefaultHypervisorOnAKVMHost(t *testing.T) {
	fcBin, _ := requireFirecracker(t)
	choice := hypervisor.Default("firecracker", "")
	t.Logf("default hypervisor: %s (%s), euid %d", choice.Name, choice.Reason, os.Geteuid())
	if choice.Name == hypervisor.QEMU {
		require.NotZero(t, os.Geteuid(), "root can create network devices")
		require.Contains(t, choice.Reason, "CAP_NET_ADMIN")
		return
	}
	require.Equal(t, hypervisor.Firecracker, choice.Name)
	require.Equal(t, fcBin, choice.FCBin)
}
