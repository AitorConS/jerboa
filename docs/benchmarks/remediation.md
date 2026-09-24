# v0.54.2 benchmark regressions

Work and validation are tracked in [the checklist](remediation-checklist.md).
The changes require rebuilding the kernel and CLI/daemon, then rebuilding guest
images; replacing only the CLI does not update kernels embedded in images.
The macOS Unix-stream transport correction also requires the companion
[firecracker-macos PR #6](https://github.com/AitorConS/firecracker-macos/pull/6)
(branch `fix/benchmark-network-backpressure`). Those changes are not contained
in this repository.
Tests used an isolated build; installed applications and services were not replaced.

## Memory and Go on ARM64

The Go failure was reproduced with anonymous pages returned by
`madvise(MADV_DONTNEED)` retaining old contents. The kernel now discards those
pages while preserving the mapping and neighbouring pages. Page-table removal
and local invalidation happen immediately; physical reclamation waits for remote
TLB shootdown. The syscall must not rendezvous with other CPUs while holding the
address-space lock: that deadlocked Linux fio during memory reclamation.
`MADV_FREE` returns `EINVAL` until its lazy-discard contract is implemented, so
callers can use their existing fallback. ARM64 TLB invalidation now includes the
required completion barriers.

The ARM64 vDSO exports the names and version expected by libc and Go and reads
the virtual counter without a system call. Its runtime image contains only the
loadable ELF segment: copying trailing debug/section data had displaced vvar.
These changes reduce timekeeping overhead in workloads that check elapsed time
on every small operation. A large-block memory result alone cannot identify
which per-operation cost dominates; measure matching before/after cases.

## Network and packages

Socket send/receive budgets are per socket, including UDP; accepted sockets
inherit their listener's settings. TCP_INFO reports congestion windows in
segments. The VirtIO network transmit queue has a bounded memory budget and
uses device-level packet drops under overload, allowing TCP to retransmit.
A completed UDP test must report packet loss as well as throughput.

Large TCP reads now return all receive-window credit, including reads above
65,535 bytes. Partial TCP writes return accepted bytes when lwIP runs out of
buffers, instead of sleeping without flushing or replaying an accepted prefix.
Unsupported TCP_INFO counters, including total retransmissions, remain zero;
iperf's zero retransmit count is not evidence of loss-free transport.

The macOS Firecracker companion patch queues complete framed packets within a
1 MiB budget, retains partial writes, and retries receiver backpressure. It
preserves VirtIO descriptors when broker IPC is full and refunds unused rate
credits. Jerboa's switch writer remains bounded by both frames and bytes so a
slow VM cannot block unrelated VMs. These changes prevent burst loss at the
previous single-frame transport queue; overload can still drop packets.

The sustained-network follow-up identified two additional software causes:

- An empty VirtIO RX ring was treated as packet loss. It now retains the one
  bounded IPC frame until the guest replenishes descriptors; an RX kick drains
  the pending input immediately. Once CPUs are parked for pause, undeliverable
  frames are explicitly dropped and counted so the snapshot barrier can finish.
- The broker polled only guest IPC and the VMM slept between device polls. Both
  now wake on inbound network readiness as well as control activity. Writable
  readiness is requested only for pending output, and a full stream queue stops
  requesting guest IPC input, avoiding a saturation busy loop.

A 60-second Linux overload control also exposed a VirtIO EVENT_IDX lost-wakeup:
when a polling TX queue remained full, its interrupt threshold was not rearmed
after the first completion batch. The tail then drained only on unrelated new
transmissions, delaying iperf's TCP control message until the test failed.
`virtqueue_fill()` now rearms each waiting batch and polls again if completion
raced the rearm. This does not change queue sizes. The failed run is retained
at Linux `runs/followup-final`; it is not counted as a completed UDP measurement.
The 60-second overload case is also a regression for control-channel completion.

With the same default quotas and one vCPU, the 20-second diagnostic progression
was 91.48 -> 210.41 -> 507.34 Mbit/s received TCP. The first change removed the
zero-progress intervals; readiness-driven processing removed the remaining
polling bottleneck. UDP at an offered 1 Gbit/s progressed from 213.49 Mbit/s
received / 78.42% loss to 521.37 Mbit/s / 47.73% loss. No queue limits or quotas
were increased.

The initial 60-second matrix completed in both directions: TCP 507.28 / 506.77
Mbit/s, UDP 100 and 400 Mbit/s with zero observed loss, and UDP at an offered
1 Gbit/s receiving 521.34 / 521.38 Mbit/s with 47.82% / 47.77% loss. These are
VM-to-VM results, not Docker parity or Internet throughput claims. The final
build measurements and hashes are recorded in the measurement record.

A subsequent 60-second repeat observed small below-quota losses at 400 Mbit/s
(0.0057% forward, 0.0108% reverse), while 100 Mbit/s remained at zero observed
loss. These small losses are not explained by the byte quota and are retained
in the results, not rounded to zero. The sampled VMM RX/IPC drop counters stayed
at zero; those counters do not cover every guest/switch drop location. This
bounded-queue implementation does not promise loss-free UDP under all scheduling
conditions.

The default 64 MiB/s quota counts Ethernet bytes in both directions. For 1420-byte
UDP payloads and 1462-byte Ethernet frames, its payload ceiling is approximately
521.45 Mbit/s, before small control traffic overhead. Thus the remaining overload
loss is expected: a 1 Gbit/s offered load cannot traverse this quota. TCP also
spends its aggregate quota on ACKs. The 100,000 packet/s cap is not the active
limit in these large-packet runs. TCP_INFO total_retrans remains unimplemented;
zero retransmissions reported by iperf is not a measured absence of retransmits.

The completed 60-second repeat used iperf 3.18, one vCPU and 512 MiB per
VM. “Reverse” exchanges sender and receiver; every case has complete receiver
JSON and no zero-progress intervals:

| Platform / direction | TCP received, Mbit/s | UDP 400M received / loss | UDP 1G received / loss |
| --- | ---: | ---: | ---: |
| macOS forward | 506.74 | 399.96 / 0.0057% | 520.68 / 47.8853% |
| macOS reverse | 506.83 | 399.95 / 0.0108% | 521.24 / 47.7935% |
| Linux forward | 704.08 | 384.36 / 3.8973% | 851.89 / 14.7871% |
| Linux reverse | 709.06 | 390.96 / 2.2590% | 856.94 / 14.3024% |

UDP at 100M had zero observed loss on macOS; Linux measured 0.0017% forward and
zero reverse. Linux does not use the companion's macOS byte quota: its observed
loss is reported without attributing it to that quota or claiming performance
parity. The final macOS build was also checked with a complete 20-second
bidirectional matrix (TCP 507.21/507.26 Mbit/s), followed by four-job fio in
both directions. Raw run identities distinguish the sustained measurements
from the final build checks.

`pkg from-docker` includes the glibc runtime libraries used for thread
cancellation and name resolution when present in the source image. These are
loaded at runtime and do not necessarily appear in DT_NEEDED. Re-extract old
packages to pick up the missing files. Arbitrary application plugins still need
explicit packaging; ELF dependency resolution cannot discover every `dlopen`.

## Storage under memory pressure

Writeback starts before the page cache exhausts memory needed for metadata and
I/O completion. Each batch is limited by pages, independent of scatter/gather
coalescing. Pending dirty ranges and queued writes hold separate page references,
so overlapping writeback cannot recycle a page that another operation still uses.
An allocation failure yields to completion/reclaim work instead of spinning
under the global cache lock.

VirtIO block request headers use a reclaimable, locked object cache backed by
contiguous pages. Previously every small header consumed an entire mapped page;
a device accepting one data segment could exhaust memory while initializing a
large extent. TFS zero-fill now consistently counts bytes in scatter/gather
buffers, and oversized scatter/gather allocations are freed without an invalid
shrink assertion.

Both native platforms completed four fio jobs per direction and verified a
1 GiB data set with 256 MiB RAM, including reboot and disk-full cases. Linux
completed two clean fio runs after the address-space deadlock fix. These tests
establish completion and data integrity for those workloads, not filesystem-wide
correctness or a throughput comparison with the original article.

The pre-existing shrink/regrow bug is addressed by coordinating the cache and
extent lifetime, rather than only changing filelength metadata. Cache writes
and file sync share a per-node writer mutex. Shrink acquires it without holding
the filesystem mutex, drains prior writeback, clears and flushes the retained
partial block, then removes or shortens extents. Sparse holes and uninitialized
extents already read as zero and do not require a data allocation on shrink. Metadata errors stop removal;
blocks are not freed if the corresponding log update failed. Removed blocks
remain reserved until old reads complete and cached copies are zeroed, preventing
reuse while an earlier read still targets them. The new length is published
before releasing the writer mutex. Background writeback defers while extents are
being changed; queued syscall completions retain asynchronous delivery.

The implementation does not allocate a zero buffer for the discarded file tail:
it clears resident pages, stores one release range per affected extent, and
releases physical blocks after the read barrier. It rechecks file length after
waiting for the mutex, because another truncation can have completed meanwhile.
The existing deferred TLB reclamation is unchanged; no synchronous shootdown was
added under the address-space lock.

The expanded MAP_SHARED regression also exposed ARM64's `pte_is_dirty()` stub,
which always returned false. Writable mappings are now conservatively considered
dirty, preserving shared writes through sync and truncation on CPUs without
hardware dirty-bit management. This can cause redundant writeback and retain
writable mapped pages longer; precise write-fault tracking remains an optimization.

`tests/guest/truncate.c` covers dirty and already-synced data, concurrent fsync,
concurrent truncations, block/page boundaries, shared mappings, O_TRUNC, invalid
lengths and read-only descriptors. `truncate_enospc.c` checks shrink/regrow on a
full 64 MiB filesystem, including a sparse file whose truncated boundary has no
allocated block. Both have reboot verification. The original reproducer
is retained as a control. Reboots in this campaign are orderly and include fsync;
these results do not establish power-loss atomicity of the entire TFS journal.

## Volumes, DNS and network lifecycle

New named volumes are created, resolved, seeded and removed through the daemon.
A client on another machine no longer sends a path under its own home directory
for the daemon to open. Removal rejects any registered VM reference, including
stopped VMs. Explicit legacy host paths remain supported.

For a legacy client-owned volume, stop and remove the VM records referring to
it, then run `jerboa volume migrate NAME` against the updated daemon. This copies
the quiescent disk into the daemon's store, preserves the source and refuses to
overwrite an existing destination. Do not copy a disk while another daemon or
process is writing it. Migration is unnecessary when both sides already use the
same volume store.

The protocol version is increased; update the CLI and daemon together. The Linux
service's ambient and bounding capability sets include `CAP_NET_BIND_SERVICE`
for guest DNS. Existing installations need the updated service unit and a daemon
restart; tests do not modify the installed service.

VM removal journals IP release before removing the VM record. Startup recovers
unfinished releases, while preserving addresses still owned by another VM and
explicit reservations. Recreating a VM with a static address no longer leaks its
previous lease.

## Backend limits and compatibility

Native QEMU on macOS supports high guest RAM through its high-memory layout and
has been exercised with 8 vCPUs and 4 GiB. The native Firecracker backend's actual
4-vCPU/2-GiB limits remain enforced; removing a validation check would not add
backend support.

`/dev/zero` is available for read/write workloads. Empty arguments and environment
values retain their quoting in the image manifest, including Redis's empty
configuration values.

Jerboa's single-process guest still cannot run fork/exec-based workloads:

- Use sysbench's threaded CPU test for a supported CPU workload. Do not label it
  as a successful stress-ng run.
- Use `fio --thread`; record any build changes needed for unsupported interfaces.
- Inspect application logs and expose application diagnostics instead of
  claiming a `docker exec` equivalent.
- Database initialization that launches subprocesses must happen before image
  construction. A preinitialized data directory does not establish that every
  PostgreSQL mode or pgbench scenario is supported.

## Comparable measurements

Run Docker and Jerboa sequentially with matching native-architecture binaries,
CPU counts, RAM, block sizes, thread counts and durations. Retain command lines,
versions, kernel identity, raw output and errors for every named case. Report
all fio jobs; reject timeouts, aborted connections and incomplete JSON.

Separate image acquisition/build from startup. Downloading a Docker image and
starting a prepared Jerboa image are different measurements. For startup, prepare
both images first, measure the first launch separately, then repeat warm
launches and report the distribution. State whether daemon/hypervisor startup,
application readiness and teardown are included. Avoid a single mixed “cold
start” ratio.

Measured matching cases used one CPU, one thread, 512 MiB RAM and a 10-second
memory test. Startup includes CLI invocation, guest execution and removal, with
the daemon running and images already prepared. Warm values are medians of ten
launches. These configurations differ from the original article's four-thread
tests; the following numbers must not be combined into a before/after ratio
with that article.

| Case | macOS Docker | macOS Jerboa | Linux Docker | Linux Jerboa |
| --- | ---: | ---: | ---: | ---: |
| Memory 1K sequential, MiB/s | 11,338.78 | 9,942.23 | 3,743.80 | 2,965.50 |
| Memory 1K random, MiB/s | 2,740.76 | 2,658.72 | 1,238.56 | 1,102.21 |
| Memory 1M sequential, MiB/s | 50,954.34 | 50,657.03 | 13,249.28 | 13,283.41 |
| Prepared warm startup, ms | 132.35 | 165.04 | 392.995 | 416.91 |

A matching original-kernel macOS control measured 1,772.61 MiB/s (1K
sequential), 1,182.93 (1K random) and 49,483.20 (1M sequential). This isolates
the small-operation improvement without claiming an equivalent increase in
large-block bandwidth. Full startup samples, kernel identities, fio job
completion and network loss are retained in [the measurement record](remediation-results.json).

## Rebuilding and reproducing the follow-up

Both dirty worktrees are required; these changes have not been published or
installed over system runtimes. Jerboa is based on `50e45ad`; the companion is
`firecracker-macos/taniwha`, branch `fix/benchmark-network-backpressure`.

On macOS, `sh scripts/build-macos.sh` builds the ARM64 kernel, native host tools,
CLI and daemon into `dist/macos-arm64`. Incremental kernel builds use
`make -C kernel -j4 kernel tools`; copy the resulting kernel into a **test**
toolset, retaining `platform.txt`, `kernel-version.txt`, `mkfs` and `boot.img`.
Compile the guest regressions with `aarch64-linux-gnu-gcc -O2 -Wall -Wextra
-Werror -static -pthread`. In the companion checkout run
`sh experiments/hvf/build-native.sh`; use its `build/macos-arm64/firecracker`
explicitly. `experiments/hvf/UNIX_STREAM.md` documents separate output directories.
Run `sh experiments/hvf/test-network-stream.sh` and
`python3 -m unittest discover -s experiments/hvf -p test_network_stream.py -v`.

Linux validation uses the isolated `/home/aitorcons/jerboa-remediation` tree:
`PATH="$PWD/toolchain/usr/bin:$PATH" make -C kernel -j2 kernel tools`, native
`cc -O2 -Wall -Wextra -Werror -static -pthread` for the C tests, and the explicitly
selected `/usr/local/bin/firecracker`. The x86 kernel is
`kernel/output/platform/pc/bin/kernel.img`. Stage it in that tree's `bin/tools`,
not in the installed service's tool directory. Its CLI/daemon were cross-built
from this checkout; the host has no Go installation. From a Go-capable machine,
use `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o OUT/jerboa ./cmd/jerboa`
and the equivalent command for `OUT/jerboad` / `./cmd/jerboad`, then copy only
those outputs to the isolated test tree.

Use `tests/guest/run.py` with explicit kernel, mkfs, Firecracker, program and
fresh work paths. The exact truncation and network matrices are documented in
[the guest README](../../tests/guest/README.md). Application runs use
`scripts/benchmark-regressions.py --bin BIN --tools TOOLS --firecracker FC
--packages PACKAGES --work NEW_DIRECTORY --cases network,disk
--network-seconds 60 --udp-rates 100M,400M,1G --network-reverse`.
Run Docker separately, if a Docker comparison is desired. The runner cleans up
its own VMs/daemon and retains raw JSON, serial logs and available VMM metrics.

Final storage validation: `/tmp/jr-truncate-delivery` and
`/tmp/jr-truncate-sparse-final` on macOS, and isolated Linux
`runs/truncate-delivery` and `runs/truncate-sparse-final`, passed both boots at
128 MiB. Linux `runs/random-delivery` passed four-writer 1 GiB byte verification
and reboot; `disk-delivery.log` rechecked the prior 1 GiB pressure disk and
ENOSPC with the final kernel. macOS pressure/random checks and the final fio
runs are recorded in the JSON. Final basic concurrent memory tests passed on
both architectures, and macOS TCP byte integrity passed.

Test VMs and daemons were stopped. Linux cleanup removed only reconstructible
`home/.jerboa/{packages,images}` copies inside our `followup-final`,
`followup-events` and `followup-disk` runs, recovering about 1.3 GiB. Their file
SHA256 inventories, logs and results remain; regression disks and original
volumes/packages were preserved. Final Linux logs and cleanup inventories also
have a local copy at `/tmp/jerboa-remediation/linux-delivery-evidence`.
