//go:build linux

package vm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCgroupLimit_Fields(t *testing.T) {
	lim := CgroupLimit{CPUShares: 256, MemoryMax: 512 * 1024 * 1024}
	require.Equal(t, uint64(256), lim.CPUShares)
	require.Equal(t, int64(512*1024*1024), lim.MemoryMax)
}

func TestNewCgroupManager(t *testing.T) {
	m := NewCgroupManager("abc123")
	require.NotNil(t, m)
}

func TestCgroupLimit_ZeroValues(t *testing.T) {
	lim := CgroupLimit{}
	require.Equal(t, uint64(0), lim.CPUShares)
	require.Equal(t, int64(0), lim.MemoryMax)
}

func TestCgroupLimit_CPUSharesRange(t *testing.T) {
	lim := CgroupLimit{CPUShares: 1}
	require.Equal(t, uint64(1), lim.CPUShares)
	lim = CgroupLimit{CPUShares: 10000}
	require.Equal(t, uint64(10000), lim.CPUShares)
}

func TestIsCgroupV2Available_Consistent(t *testing.T) {
	result := IsCgroupV2Available()
	require.Equal(t, result, IsCgroupV2Available())
}
