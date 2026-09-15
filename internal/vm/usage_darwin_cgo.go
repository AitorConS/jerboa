//go:build darwin && cgo

package vm

/*
#include <libproc.h>
#include <mach/mach_time.h>
#include <sys/resource.h>
#include <sys/proc_info.h>
static int jerboa_usage(int pid, struct rusage_info_v2 *usage, struct proc_bsdinfo *bsd) {
 struct proc_bsdinfo before;
 int size = sizeof(struct proc_bsdinfo);
 if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &before, size) != size ||
     proc_pid_rusage(pid, RUSAGE_INFO_V2, (rusage_info_t *)usage) != 0 ||
     proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, bsd, size) != size) {
  return -1;
 }
 if (before.pbi_pid != bsd->pbi_pid ||
     before.pbi_start_tvsec != bsd->pbi_start_tvsec ||
     before.pbi_start_tvusec != bsd->pbi_start_tvusec) {
  return -1;
 }
 return 0;
}
static int jerboa_children(int pid, int *pids, int capacity) {
 return proc_listchildpids(pid, pids, capacity * sizeof(int));
}
static uint64_t jerboa_abstime_ns(uint64_t ticks) {
 mach_timebase_info_data_t timebase;
 if (mach_timebase_info(&timebase) != KERN_SUCCESS || timebase.denom == 0) {
  return 0;
 }
 __uint128_t ns = (__uint128_t)ticks * timebase.numer / timebase.denom;
 return ns > UINT64_MAX ? UINT64_MAX : (uint64_t)ns;
}
*/
import "C"
import (
	"fmt"
	"unsafe"
)

func readDarwinUsage(pid int) (darwinUsage, error) {
	var u C.struct_rusage_info_v2
	var b C.struct_proc_bsdinfo
	if C.jerboa_usage(C.int(pid), &u, &b) != 0 {
		return darwinUsage{}, fmt.Errorf("proc_pid_rusage(%d) failed", pid)
	}
	return darwinUsage{
		pid:   int(b.pbi_pid),
		ppid:  int(b.pbi_ppid),
		start: uint64(b.pbi_start_tvsec)*1_000_000 + uint64(b.pbi_start_tvusec),
		cpuNS: uint64(C.jerboa_abstime_ns(C.uint64_t(u.ri_user_time + u.ri_system_time))),
		rss:   int64(u.ri_resident_size),
		disk:  int64(u.ri_diskio_bytesread + u.ri_diskio_byteswritten),
	}, nil
}

func listDarwinChildren(pid int) ([]int, error) {
	for capacity := 16; capacity <= 4096; capacity *= 2 {
		pids := make([]C.int, capacity)
		count := int(C.jerboa_children(C.int(pid), (*C.int)(unsafe.Pointer(&pids[0])), C.int(capacity)))
		if count < 0 {
			return nil, fmt.Errorf("proc_listchildpids(%d) failed", pid)
		}
		if count < capacity {
			result := make([]int, 0, count)
			for _, child := range pids[:count] {
				if child > 0 {
					result = append(result, int(child))
				}
			}
			return result, nil
		}
	}
	return nil, fmt.Errorf("process tree rooted at %d exceeds 4096 children", pid)
}

const darwinStatsSource = "darwin-libproc"
