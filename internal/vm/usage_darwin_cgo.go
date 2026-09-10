//go:build darwin && cgo

package vm

/*
#include <libproc.h>
#include <sys/resource.h>
static int jerboa_usage(int pid, struct rusage_info_v2 *usage) {
 return proc_pid_rusage(pid, RUSAGE_INFO_V2, (rusage_info_t *)usage);
}
*/
import "C"
import "fmt"

func readDarwinUsage(pid int) (darwinUsage, error) {
	var u C.struct_rusage_info_v2
	if C.jerboa_usage(C.int(pid), &u) != 0 {
		return darwinUsage{}, fmt.Errorf("proc_pid_rusage(%d) failed", pid)
	}
	return darwinUsage{start: uint64(u.ri_proc_start_abstime), cpuNS: uint64(u.ri_user_time + u.ri_system_time), rss: int64(u.ri_resident_size), disk: int64(u.ri_diskio_bytesread + u.ri_diskio_byteswritten)}, nil
}

const darwinStatsSource = "darwin-libproc"
