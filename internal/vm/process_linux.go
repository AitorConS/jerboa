package vm

import (
	"bytes"
	"fmt"
	"os"
)

func processOwnsVM(pid int, vmID string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte(qmpSocketPath(vmID)))
}
