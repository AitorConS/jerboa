# Benchmark regression checklist

Scope: failures reported with Jerboa v0.54.2 on Linux and macOS ARM64.
Completed means implementation and relevant validation passed; it does not
mean Docker feature or performance parity. See [results and limitations](remediation.md)
and the [measurement record](remediation-results.json).

- [x] Preserve empty manifest arguments and environment values; test serialization and guest argv.
- [x] Grant the Linux daemon permission to bind DNS; test generated units and an isolated native Linux service binding port 53 as `jerboa`.
- [x] Release network leases on manual and automatic VM removal, including recovery; test reuse and concurrency.
- [x] Make named volumes daemon-owned, including remote clients and explicit migration of existing client-owned volumes.
- [x] Fix macOS Firecracker Unix transport burst loss with bounded buffering and retry; test framing, saturation, reconnect, snapshots and native VM networks.
- [x] Correct TCP_INFO units and socket buffer semantics; complete TCP/UDP runs on both guest architectures.
- [x] Fix ARM64 Go memory corruption; verify anonymous-page discard and Go workloads with 1 and 4 vCPUs.
- [x] Fix reported write stalls, allocation failures and scatter/gather assertions under memory pressure; verify four-job fio, data integrity after restart and disk-full handling.
- [x] Diagnose small-block memory overhead; correct and measure vDSO timekeeping.
- [x] Review CPU/RAM limits against backend capabilities; test supported QEMU limits and retain actual native Firecracker limits.
- [x] Add `/dev/zero` read/write support and document alternatives for fork/exec-dependent tools.
- [x] Repeat matching memory/startup benchmarks with explicit case names, complete-run checks and prepared images.
- [x] Run affected Go/kernel checks and Linux/macOS integration tests; record evidence and remaining limitations.

## Validation completed

- Full `go test -race ./...`: macOS ARM64 and Linux ARM64 (Docker).
- `make test-kernel`: host kernel unit tests, vDSO ELF layout, HTTP poll/select and UDP.
- Installer fixture suite: Linux systemd and macOS installation paths.
- Native guests: macOS ARM64/HVF and Linux x86_64/KVM. Empty argv,
  concurrent anonymous-page discard, socket options and `/dev/zero` passed.
- Storage: 1 GiB with 256 MiB RAM, full readback, reboot verification, ENOSPC,
  deletion of the filler and re-verification; four-thread random-write byte
  verification and reboot; four fio jobs per direction for 60 seconds. Linux
  completed two clean fio passes after the final deadlock correction.
- Go HTTP: 60-second native HVF runs with default asynchronous preemption at
  1 and 4 vCPUs. QEMU/HVF also completed an 8-vCPU, 4-GiB run.
- Native macOS volumes: three guest boots and daemon restart.
- Native macOS networks: static IP, service/alias DNS, isolation, policies,
  Compose, port reuse and daemon SIGKILL/restart recovery.
- Firecracker transport: ASan/UBSan framing/backpressure tests, Seatbelt
  capability test, budget-refund test and real HVF API snapshot/reconnect test.
- iperf3: complete TCP/UDP runs on both native platforms. Redis: 100,000 SET
  requests on each platform, using the empty `--save` argument.

The installed runtimes/services and the original benchmark volumes were not
replaced. Test daemons and VMs have been stopped; isolated test logs remain.

## Findings closed by this follow-up

- [x] Improve sustained macOS VM-to-VM network throughput and overload loss.
  Completing a connection is not a claim that its throughput matches Docker.
- [x] Fix the pre-existing TFS shrink/regrow bug exposed by the additional
  `tests/guest/known_issues/truncate.c` reproducer. Old tail bytes reappear after
  shrinking and extending a file. The original control failed; the corrected kernels pass on both architectures.

Fork/exec support and native Firecracker support beyond 4 vCPUs/2 GiB are
architectural follow-up work, not completed fixes in this patch.

## Follow-up ownership: network and TFS (2026-09-24)

- [x] Review both dirty worktrees and previous evidence; preserve existing fixes.
- [x] Reproduce shrink/regrow with the current kernel and retain logs.
- [x] Coordinate truncation with cached pages, queued writes and extent metadata; propagate failures safely.
- [x] Add boundary, dirty/writeback, reboot, low-memory and error regression coverage.
- [x] Validate TFS and existing storage/memory regressions on macOS ARM64 and Linux x86_64.
- [x] Locate sustained-network stalls/loss with per-stage evidence and bounded queues.
- [x] Fix demonstrated software causes and add targeted transport regressions.
- [x] Complete TCP and UDP measurements at below-quota and overload rates on both platforms; report loss and stalls.
- [x] Record exact build/run instructions, results and remaining limits for both repositories.

Final evidence: macOS `/tmp/jr-truncate-delivery` and
`/tmp/jr-truncate-sparse-final`, and Linux `runs/truncate-delivery` and
`runs/truncate-sparse-final`, pass both initial and reboot verification at
128 MiB. Concurrent memory tests, four random writers with 1 GiB byte checks,
reboot, ENOSPC and four-job fio also pass. Sustained 60-second network matrices
are `/tmp/jr-followup-final` and Linux `runs/followup-events`; the final macOS
build additionally completed `/tmp/jr-delivery`. Raw values, hashes and known
limits are retained in the report and JSON. No installed runtime was replaced.
