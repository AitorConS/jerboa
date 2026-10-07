//go:build linux

package vm

import (
	"bufio"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHugePagesAvailable(t *testing.T) {
	meminfo := "MemTotal: 4096000 kB\nHugePages_Total: 560\nHugePages_Free: 540\nHugePages_Rsvd: 20\nHugePages_Surp: 0\nHugepagesize: 2048 kB\n"
	free, size := hugePagesAvailable(bufio.NewScanner(strings.NewReader(meminfo)))
	require.Equal(t, int64(520), free)
	require.Equal(t, int64(2048), size)

	free, size = hugePagesAvailable(bufio.NewScanner(strings.NewReader("HugePages_Free: 0\nHugePages_Rsvd: 3\nHugepagesize: 2048 kB\n")))
	require.Equal(t, int64(0), free)
	require.Equal(t, int64(2048), size)
}

func TestFCHugePagesRejectsOddMemory(t *testing.T) {
	require.Empty(t, fcHugePages(1023))
	require.Empty(t, fcHugePages(0))
}
