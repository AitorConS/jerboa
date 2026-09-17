---
layout: default
title: macOS VM statistics
nav_order: 15
---

# macOS VM statistics

Jerboa reports native Firecracker statistics from the host process tree rooted
at the per-VM API supervisor. The tree includes the VMM and its broker/authority
descendants; `source` is `darwin-libproc-tree`. Native QEMU retains its
single-process `darwin-libproc` accounting.

`cpu_pct` is the sum of host CPU-time deltas for the live tree divided by wall
time. Mach absolute ticks are converted with the host timebase; 100% means one
logical host CPU, and the value is capped at the host's total logical CPU
capacity. A collector's first sample establishes a zero-CPU baseline. Only a
child whose kernel creation time falls inside the current sampling interval
contributes its lifetime CPU from zero. An older child omitted by a transient
read/enumeration race receives a safe new baseline when it reappears instead of
producing a lifetime-sized spike. CPU consumed by a very short process that both
starts and exits between samples cannot be seen.

`mem_bytes` is the sum of current host resident-set bytes (RSS) for the live
tree. It is neither the configured guest RAM nor a guest-internal used-memory
metric. The VMM RSS can include resident guest-memory mappings, while untouched
or reclaimed guest pages need not be resident. `disk_bytes` sums cumulative
host process disk-I/O bytes for the currently live tree. Network byte counters
come from Jerboa's per-VM native network path for VMs on a named network. A
Firecracker/HVF VM without `--network` uses the VMM's own slirp, so its counters
are the guest NIC's `net_rx_bytes_total` and `net_tx_bytes_total` from the HVF
`GET /metrics` API; an unavailable sample reads as zero.

Every process is keyed by PID and kernel creation time. The root identity is
captured when the collector is installed and a mismatch returns fallback zeros,
so a reused supervisor PID cannot attach statistics to an unrelated process.
Each traversal verifies the child's current parent and creation time, ignores
duplicate enumeration results, and replaces the identity map after every sample
so exited children cannot leak collector state. A restarted VM gets a new
collector and identity baseline; a daemon-adopted live VM also starts with a
fresh baseline while retaining the existing process tree.

Native tree accounting requires cgo/libproc. A macOS build without cgo returns
fallback zeros instead of using an identity-unsafe `ps` estimate.

## Reproduce

The focused unit regressions cover real child discovery in a test-owned process
group, duplicate children, child exit, PID reuse, a replacement child, a
three-sample transient omission and adoption baselining:

```sh
../firecracker-macos/experiments/hvf/build/guest-tools/go/bin/go test -race \
  ./internal/vm -run 'TestDarwinStats|TestNativeStats' -count=1 -v
```

The isolated Firecracker acceptance builds a temporary ARM64 guest, applies
four-vCPU and 192 MiB guest loads, compares Jerboa with host `ps`, exercises a
failed-guest restart and kills/restarts only its own daemon for adoption:

```sh
../firecracker-macos/experiments/hvf/build/guest-tools/go/bin/go build \
  -o dist/sol-closure/stats/bin/jerboa ./cmd/jerboa
../firecracker-macos/experiments/hvf/build/guest-tools/go/bin/go build -race \
  -o dist/sol-closure/stats/bin/jerboad ./cmd/jerboad
ln -sfn ../../../macos-arm64/tools dist/sol-closure/stats/bin/tools
JERBOA_TEST_BIN="$PWD/dist/sol-closure/stats/bin" \
JERBOA_FIRECRACKER_BIN="$PWD/../firecracker-macos/experiments/hvf/build/astra-shared-network/review-fixes/package/bin/firecracker" \
JERBOA_TEST_OUTPUT="$PWD/dist/sol-closure/stats/evidence" \
python3 scripts/test-firecracker-stats.py
```

The script uses a unique temporary HOME, daemon socket, port, image store and
VMs, and removes only those resources. `JERBOA_TEST_BIN`,
`JERBOA_FIRECRACKER_BIN`, `JERBOA_TEST_OUTPUT` and optional `JERBOA_TEST_GO`
make the tested bundle and retained evidence explicit.
