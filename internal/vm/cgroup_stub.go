//go:build !linux

package vm

import "fmt"

type CgroupLimit struct {
	CPUShares uint64
	MemoryMax int64
}

type CgroupManager struct{}

func NewCgroupManager(_ string) *CgroupManager {
	return &CgroupManager{}
}

func (m *CgroupManager) Apply(_ int, _ CgroupLimit) error {
	return fmt.Errorf("cgroup v2 is only available on Linux")
}

func (m *CgroupManager) Remove() error {
	return nil
}

func IsCgroupV2Available() bool {
	return false
}
