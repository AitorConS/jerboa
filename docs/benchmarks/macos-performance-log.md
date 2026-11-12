# macOS performance log

What has been tried to make Jerboa faster on Apple Silicon (the Firecracker
fork on Hypervisor.framework, `firecracker-macos`), with the verdict of each
experiment. An entry is one line: add yours when an experiment ends, whichever
way it went. A rejected idea is not forbidden; it needs new data to be tried
again.

Numbers are from an M5 with 4 vCPUs and 2 GiB unless stated. Repeated runs of
one build differ by about 5% (sequential write, network, start-up) and 10 to
15% (random write). Random write also drops with Docker Desktop open or a
busy performance core.

## Start-up

`jerboa run --rm` of a guest that prints one line: 92 ms before 2026-11-12,
42 ms after.

| Change | Verdict | Effect |
|---|---|---|
| No full flush (`F_FULLFSYNC`) of the staged kernel and the workspace lease | kept (firecracker-macos #27) | 21 ms |
| Supervisor loop woken by kqueue instead of every 5 ms | kept (#27) | about 20 ms across start and exit |
| VMM, broker and gate install their sandboxes at the same time | kept (#28) | 7 ms |
| No `--check-config` process before every start | kept (jerboa #238) | 3 ms |
| Daemon polls the supervisor every 1 ms instead of 5 ms | rejected | 1 ms, for five times the requests |

Why a start still costs what it does: each of the two remaining processes
takes about 4 ms from exec to `main`, and each sandbox about 4.5 ms.
`GET /metrics` on the VMM's socket reports the steps of the last start
(`startup_*_microseconds`).

The lease marker is written with `fsync`, not a full flush. A power cut right
after a start can leave a marker recovery cannot read; it then leaves that
temporary directory in place. Nothing else depends on the marker.

## Disk

| Change | Verdict |
|---|---|
| `preadv`/`pwritev` on the guest's segments | kept |
| Block worker pool of 4; only writes of 64 KiB or less run concurrently | kept |
| Host `fsync` (not full) every 64 MiB written | kept |
| Page maps for sequential extents (kernel) | kept |
| Workers woken only when there is work | kept |
| Block interrupt suppression | rejected: reverted after an incomplete write; `EVENT_IDX` was never offered |
| Virtqueue notify without the lock | rejected |
| Throttle without FLUSH; no FLUSH on reclaim syncs | rejected |
| `F_NOCACHE` on disks | rejected |
| Writeback scan every 1 s | rejected |
| Skipping flushes with nothing to cover | rejected |
| Shared reclaim syncs; decoupled reclaim | rejected |
| Worker counts 1, 2, fixed 4, 8, 16 | rejected in favor of the adaptive pool |
| Every write concurrent | rejected |
| Pacing every 16 MiB or 256 MiB | rejected in favor of 64 MiB |
| Page maps for every first write | rejected |
| Wide `memcpy` | rejected |

Open: random write spends most of its time waiting on reclaim syncs; a
redesign of the throttle is the known next step and the risky one.

Measured and not pursued (2026-11-12): `EVENT_IDX` and indirect descriptors on
the block device. Random write 4k x 4 already costs 0.79 vCPU exits per block
request, about 5,000 exits a second: there is no time to win there.

## Network

| Change | Verdict |
|---|---|
| Interrupt suppression on the network queues | kept |
| Buffered reader and read-ahead on the stream | kept |
| MTU 9000 and 1 MiB windows | kept |
| Direct VM-to-VM peering, with TSO and checksum offload on it | kept |
| 1 MiB socket buffers; the VMM owns the switch link | kept |
| Adaptive wake after 32 mid-frame polls; `net_backend_kick` | kept |
| GSO towards the gateway | kept |
| Event-driven RX wake | rejected three times, up to 46% slower |
| RX on its own VMM thread | rejected, 19% slower |
| Batching in the Go switch | rejected |
| MTU 16000 | rejected |
| TSO on the switch path by segmenting in the VMM | rejected |
| 20 µs poll; `nice` | rejected |
| MTU 9000 on the slirp link of a VM outside a network | kept (firecracker-macos #33, jerboa #242): published port 4.9 to 9.7 Gbit/s in, 2.1 to 9.0 out |

Open: VM-to-host bulk TCP is the largest gap to Docker and needs a path that
does not copy through user space twice.

Measured, not yet acted on (2026-11-12): bulk TCP through slirp at MTU 9000
costs 0.35 to 0.43 vCPU exits per packet, about 50,000 a second. `EVENT_IDX`
on the network queues would remove part of those; the expected gain is a few
percent and it has not been tried. All vCPU exits and the device loop share
one lock; its cost with several busy vCPUs has not been measured.

## Method

- Compare builds with `scripts/benchmark-compare.py` (see
  [Benchmarks](../benchmarks.md#comparing-two-builds)): interleaved, three
  rounds per build, five before a decision that is close.
- Quit Docker Desktop. Let the machine rest a minute between rounds.
- A change that removes a flush or reorders device completion also runs the
  VMM's durability suite (`experiments/hvf/durability` in firecracker-macos).
