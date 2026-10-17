//go:build linux

package vm

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// fcHugePages backs guest memory with 2 MiB huge pages when the host has
// enough of them free. Every first touch of a 4 KiB guest page otherwise
// costs a host page fault, which dominates write-heavy guests: on a nested
// KVM host, a 1000 MiB sequential write in a 1 GiB guest ran at 50-64 MiB/s
// with 4 KiB pages and 136-137 MiB/s with huge pages. Hosts reserve huge
// pages explicitly (vm.nr_hugepages); without enough free ones Firecracker
// could not allocate guest memory, so the field is then left out.
func fcHugePages(memMiB int) string {
	if memMiB <= 0 || memMiB%2 != 0 {
		return ""
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	free, size := hugePagesAvailable(bufio.NewScanner(f))
	if size != 2048 || free*size < int64(memMiB)*1024 {
		return ""
	}
	return "2M"
}

// hugePagesAvailable returns the free, unreserved huge pages and their size
// in KiB from /proc/meminfo contents.
func hugePagesAvailable(sc *bufio.Scanner) (free, sizeKiB int64) {
	var rsvd int64
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "HugePages_Free:":
			free = v
		case "HugePages_Rsvd:":
			rsvd = v
		case "Hugepagesize:":
			sizeKiB = v
		}
	}
	if free -= rsvd; free < 0 {
		free = 0
	}
	return free, sizeKiB
}
