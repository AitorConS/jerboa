//go:build linux || (darwin && arm64)

package vm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func cmdline(args ...string) []byte { return []byte(strings.Join(args, "\x00") + "\x00") }

// A VMM that outlived the daemon is recognized by the control socket named
// after its VM: QEMU's QMP socket and Firecracker's API socket. Before,
// only QEMU was, and a Firecracker VM was recorded as stopped while its
// process went on running.
func TestCmdlineOwnsVM(t *testing.T) {
	const qmp, fc = "/run/jerboa/qmp-abc123.sock", "/run/jerboa/fc-abc123.sock"
	for name, tc := range map[string]struct {
		cmdline []byte
		want    bool
	}{
		"qemu":                   {cmdline("qemu-system-x86_64", "-m", "256M", "-qmp", "unix:"+qmp+",server,nowait"), true},
		"firecracker":            {cmdline("firecracker", "--api-sock", fc, "--config-file", "/run/jerboa/fc-abc123-config.json"), true},
		"firecracker, last arg":  {cmdline("firecracker", "--api-sock", fc), true},
		"through the daemon":     {cmdline("jerboad", "__jerboa-vmm-exec", "firecracker", "--api-sock", fc), true},
		"another vm's qemu":      {cmdline("qemu-system-x86_64", "-qmp", "unix:/run/jerboa/qmp-other.sock,server,nowait"), false},
		"another vm's fc":        {cmdline("firecracker", "--api-sock", "/run/jerboa/fc-abc1234.sock"), false},
		"socket of a longer id":  {cmdline("firecracker", "--api-sock", fc+".bak"), false},
		"path but not the flag":  {cmdline("cat", fc), false},
		"flag without its value": {cmdline("firecracker", "--api-sock"), false},
		"unrelated process":      {cmdline("sleep", "300"), false},
		"empty":                  {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, cmdlineOwnsVM(tc.cmdline, qmp, fc))
		})
	}
	assert.False(t, cmdlineOwnsVM(cmdline("firecracker", "--api-sock", ""), "", ""), "no socket paths, nothing matches")
}
