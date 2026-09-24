# Guest regression tests

These tests boot the locally built kernel in an isolated VM. They do not use the
installed daemon or modify existing images and volumes. Keep each work directory
to inspect its `console.log` and disk; never reuse another workload's directory.

Compile the C files as static Linux executables for the guest architecture:

```sh
cc -O2 -Wall -Wextra -static -pthread benchmark_regressions.c -o basic
cc -O2 -Wall -Wextra -static -pthread clock.c -o clock
cc -O2 -Wall -Wextra -static -pthread disk_pressure.c -o disk-pressure
cc -O2 -Wall -Wextra -static -pthread random_write.c -o random-write
cc -O2 -Wall -Wextra -static -pthread tcp_stream.c -o tcp-stream
```

On macOS use a Linux cross compiler or a Linux build container. Native macOS
executables cannot run in the guest. On Linux, Firecracker needs access to KVM;
these standalone cases do not need a TAP device.

Run from the repository root, substituting paths to the local tools:

```sh
python3 tests/guest/run.py --kernel "$KERNEL" --mkfs "$MKFS" \
  --firecracker "$FIRECRACKER" --program ./basic --work /tmp/guest-basic \
  before '' after ''
python3 tests/guest/run.py --kernel "$KERNEL" --mkfs "$MKFS" \
  --firecracker "$FIRECRACKER" --program ./clock --work /tmp/guest-clock \
  --expect 'CLOCK PASS'
python3 tests/guest/run.py --kernel "$KERNEL" --mkfs "$MKFS" \
  --firecracker "$FIRECRACKER" --program ./disk-pressure --work /tmp/guest-disk \
  --expect 'DISK WRITE PASS'
python3 tests/guest/run.py --kernel "$KERNEL" --mkfs "$MKFS" \
  --firecracker "$FIRECRACKER" --program ./disk-pressure --work /tmp/guest-disk \
  --reuse-disk --expect 'DISK FULL PASS'
```

`random_write.c` uses four threads and four preallocated 256 MiB files. Run it
with the same default 256 MiB RAM and a fresh work directory, expecting
`RANDOM WRITE PASS`; boot again with `--reuse-disk`, expecting
`RANDOM RESTART PASS`. Both passes verify every byte. A timeout, crash or missing
completion marker fails the run.

`clock.c` resolves the architecture's vDSO symbol directly and checks it against
system calls from multiple threads. The host-side generator test also checks
that ELF debug sections cannot displace the vvar page:

```sh
python3 tests/guest/test_vdsogen.py kernel/output/tools/bin/vdsogen
```

For the original applications, `scripts/benchmark-regressions.py` creates its own
daemon, home directory, images, network and volumes. It requires explicitly
supplied local tools and packages. It checks all four fio jobs and complete
iperf3 intervals; partial results are failures. See its `--help` for arguments.

`tcp_stream.c` checks a 16 MiB stream byte for byte, using large writes and
128 KiB reads to exercise send-buffer limits and receive-window credit. Run it
with a fresh work directory and `--expect 'TCP STREAM PASS'`.

The application runner also accepts `--cases memory,startup --docker-image
bench-tools:latest`. The Docker image must already be present and contain the
same native sysbench binary. Startup runs include the CLI, guest execution and
removal, with the daemon already running and both images prepared. The first
launch is separate from ten warm launches. Metadata and results go to
`results.json`; application and daemon logs stay in the work directory.

For Redis, add `redis` to `--cases`, supply a `redis:8.0.2` package, and make
`redis-benchmark` available on the host (or pass an executable wrapper through
`--redis-benchmark`). The server uses `--save "" --appendonly no`. The runner
waits for a RESP PING response on the published port, then requires all 100,000
SET requests to complete. Test networks and daemon resources are isolated and
removed on exit; failed disk volumes and logs remain available for inspection.

## Truncation regressions

Compile `truncate.c` and `truncate_enospc.c` with the same static Linux compiler.
`truncate.c` checks 16 dirty shrink/regrow cycles over 32 MiB with concurrent
fsync, simultaneous truncations, page/block boundary cuts, MAP_SHARED contents, O_TRUNC, negative lengths
and read-only descriptors. Run with 128 MiB RAM and expect `TRUNCATE PASS`, then
boot the same disk with `--reuse-disk` and expect `TRUNCATE RESTART PASS`.
`truncate_enospc.c` uses `--disk-size 64M --memory 128`: it fills the disk to
ENOSPC, shrinks and regrows both an allocated file and a sparse file, and checks
every byte. Expect
`TRUNCATE ENOSPC PASS`, then `TRUNCATE ENOSPC RESTART PASS` after reboot.
The original, smaller reproducer remains at `known_issues/truncate.c` as a
before/after control; it is no longer the full regression matrix.

For sustained network measurements use `--network-seconds 60 --udp-rates
100M,400M,1G --network-reverse`. The application runner requires complete receiver
results and reports measured loss; completion does not imply meeting the offered
rate. On macOS it also records Firecracker metrics once per second in
`network-metrics.json`. Default native quotas remain enabled. Run measurements
without other benchmark guests or Docker workloads on that host.
