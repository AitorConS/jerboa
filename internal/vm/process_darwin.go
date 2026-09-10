package vm

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"time"
)

func processOwnsVM(pid int, vmID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	return err == nil && bytes.Contains(data, []byte("qemu-system-aarch64")) && bytes.Contains(data, []byte("unix:"+qmpSocketPath(vmID)+",server,nowait"))
}
