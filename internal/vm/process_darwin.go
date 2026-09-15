package vm

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"time"
)

func processOwnsVM(pid int, vmID string) bool {
	data, err := darwinProcessCommand(pid)
	return err == nil && ((bytes.Contains(data, []byte("qemu-system-aarch64")) && bytes.Contains(data, []byte("unix:"+qmpSocketPath(vmID)+",server,nowait"))) ||
		nativeFCSupervisorCommand(data, vmID))
}

func processIsNativeFCSupervisor(pid int, vmID string) bool {
	data, err := darwinProcessCommand(pid)
	return err == nil && nativeFCSupervisorCommand(data, vmID)
}

// nativeFCSupervisorCommand matches a booting supervisor (with a config file)
// and a snapshot-restore supervisor, which is launched with only its
// VM-specific API socket and must end there.
func nativeFCSupervisorCommand(data []byte, vmID string) bool {
	flag := "--api-sock " + nativeFCSocketPath(vmID)
	line := bytes.TrimSpace(data)
	return bytes.Contains(line, []byte(flag+" --config-file ")) || bytes.HasSuffix(line, []byte(" "+flag))
}

func darwinProcessCommand(pid int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "/bin/ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
}
