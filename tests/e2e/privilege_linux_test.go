//go:build e2e && linux

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
)

var netAdmin struct {
	once sync.Once
	err  error
}

// requireGuestNetworking makes sure this process, and so the daemons it
// starts, may create network devices: on Linux every guest network is a
// bridge with a tap per VM, which needs CAP_NET_ADMIN. The suite gets it by
// running as root or from a runner service that holds it as an ambient
// capability (as the jerboad systemd unit does).
func requireGuestNetworking(t *testing.T) {
	t.Helper()
	netAdmin.once.Do(func() {
		probe := fmt.Sprintf("jbe2e-p%d", os.Getpid()%10000)
		if out, err := exec.Command("ip", "link", "add", probe, "type", "dummy").CombinedOutput(); err != nil {
			netAdmin.err = fmt.Errorf("ip link add: %w: %s", err, out)
			return
		}
		_ = exec.Command("ip", "link", "del", probe).Run()
	})
	if netAdmin.err != nil {
		missing(t, "guest networking needs CAP_NET_ADMIN (run the suite as root, or give the runner service AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW): %v", netAdmin.err)
	}
}
