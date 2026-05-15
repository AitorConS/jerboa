//go:build linux

package vm

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const cgroupBase = "/sys/fs/cgroup"

type CgroupLimit struct {
	CPUShares uint64
	MemoryMax int64
}

type CgroupManager struct {
	vmID string
	path string
}

func NewCgroupManager(vmID string) *CgroupManager {
	return &CgroupManager{
		vmID: vmID,
		path: filepath.Join(cgroupBase, "uni", vmID),
	}
}

func (m *CgroupManager) Apply(pid int, limits CgroupLimit) error {
	if err := os.MkdirAll(m.path, 0o755); err != nil {
		return fmt.Errorf("cgroup mkdir %s: %w", m.path, err)
	}
	if limits.CPUShares > 0 {
		if err := os.WriteFile(filepath.Join(m.path, "cpu.weight"), []byte(strconv.FormatUint(limits.CPUShares, 10)), 0o644); err != nil {
			return fmt.Errorf("cgroup set cpu.weight: %w", err)
		}
		slog.Info("cgroup: set cpu weight", "vm_id", m.vmID, "weight", limits.CPUShares)
	}
	if limits.MemoryMax > 0 {
		if err := os.WriteFile(filepath.Join(m.path, "memory.max"), []byte(strconv.FormatInt(limits.MemoryMax, 10)), 0o644); err != nil {
			return fmt.Errorf("cgroup set memory.max: %w", err)
		}
		slog.Info("cgroup: set memory.max", "vm_id", m.vmID, "bytes", limits.MemoryMax)
	}
	if err := os.WriteFile(filepath.Join(m.path, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("cgroup move pid %d: %w", pid, err)
	}
	slog.Info("cgroup: moved pid", "vm_id", m.vmID, "pid", pid)
	return nil
}

func (m *CgroupManager) Remove() error {
	procs, err := os.ReadFile(filepath.Join(m.path, "cgroup.procs"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cgroup read procs: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(procs)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rootProcs := filepath.Join(cgroupBase, "cgroup.procs")
		if err := os.WriteFile(rootProcs, []byte(line), 0o644); err != nil {
			slog.Warn("cgroup: failed to move pid to root", "vm_id", m.vmID, "pid", line, "err", err)
		}
	}
	if err := os.Remove(m.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cgroup rmdir %s: %w", m.path, err)
	}
	slog.Info("cgroup: removed", "vm_id", m.vmID)
	return nil
}

func IsCgroupV2Available() bool {
	_, err := os.Stat(filepath.Join(cgroupBase, "cgroup.controllers"))
	return err == nil
}
