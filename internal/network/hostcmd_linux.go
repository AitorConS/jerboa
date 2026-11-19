package network

import (
	"os/exec"

	"github.com/AitorConS/jerboa/internal/childproc"
)

// hostCommand starts a host networking tool (ip, nft, iptables, sysctl). It
// needs the daemon's network capabilities, but none of its credentials.
func hostCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...) //nolint:gosec,noctx // a fixed host tool; callers pass validated arguments
	cmd.Env = childproc.Inherited()
	return cmd
}
