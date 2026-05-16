//go:build linux

package vm

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCgroupManager_Apply_WritesFiles(t *testing.T) {
	if !IsCgroupV2Available() {
		t.Skip("cgroup v2 not available on this system")
	}
	dir := t.TempDir()
	m := NewCgroupManager("testvm")
	m.path = dir
	limits := CgroupLimit{CPUShares: 512, MemoryMax: 1024 * 1024}
	err := m.Apply(1234, limits)
	require.NoError(t, err)
	weight, err := os.ReadFile(filepath.Join(dir, "cpu.weight"))
	require.NoError(t, err)
	require.Equal(t, "512", string(weight))
	memMax, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	require.NoError(t, err)
	require.Equal(t, "1048576", string(memMax))
	procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	require.NoError(t, err)
	require.Equal(t, "1234", string(procs))
}

func TestCgroupManager_Remove_CleansUp(t *testing.T) {
	if !IsCgroupV2Available() {
		t.Skip("cgroup v2 not available on this system")
	}
	dir := t.TempDir()
	m := NewCgroupManager("testvm")
	m.path = dir
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(""), 0o644))
	require.NoError(t, m.Remove())
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "expected cgroup directory to be removed, but it still exists: %s", dir)
}

func TestLinux_CgroupManager_Apply_CPUSharesOnly(t *testing.T) {
	if !IsCgroupV2Available() {
		t.Skip("cgroup v2 not available on this system")
	}
	dir := t.TempDir()
	m := NewCgroupManager("testvm")
	m.path = dir
	err := m.Apply(5678, CgroupLimit{CPUShares: 100})
	require.NoError(t, err)
	weight, err := os.ReadFile(filepath.Join(dir, "cpu.weight"))
	require.NoError(t, err)
	require.Equal(t, "100", string(weight))
}

func TestLinux_CgroupManager_Apply_MemoryOnly(t *testing.T) {
	if !IsCgroupV2Available() {
		t.Skip("cgroup v2 not available on this system")
	}
	dir := t.TempDir()
	m := NewCgroupManager("testvm")
	m.path = dir
	err := m.Apply(9999, CgroupLimit{MemoryMax: 2048})
	require.NoError(t, err)
	memMax, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	require.NoError(t, err)
	require.Equal(t, "2048", string(memMax))
}

func TestLinux_CgroupManager_Apply_PIDAsString(t *testing.T) {
	if !IsCgroupV2Available() {
		t.Skip("cgroup v2 not available on this system")
	}
	dir := t.TempDir()
	m := NewCgroupManager("testvm")
	m.path = dir
	require.NoError(t, m.Apply(42, CgroupLimit{}))
	procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(42), string(procs))
}

func TestLinux_IsCgroupV2Available_True(t *testing.T) {
	require.True(t, IsCgroupV2Available())
	require.FileExists(t, "/sys/fs/cgroup/cgroup.controllers")
}
