---
layout: default
title: PostgreSQL ARM64 on Nanos
nav_order: 12
---

# PostgreSQL ARM64 on Nanos

Jerboa can run a real PostgreSQL server on an ARM64 Nanos guest, but it cannot use an unmodified upstream PostgreSQL binary. Nanos is a single-application unikernel: it does not provide Unix process creation (`fork`/`exec`) or the ordinary System V shared-memory/process model on which stock PostgreSQL relies.

This repository therefore pins the experimental threaded PostgreSQL fork at commit `d5933309617388a7eb6b2e195352054edf438cb7` (PostgreSQL `11devel`, October 2018). That commit is still the fork's `master` HEAD: there is no newer maintained code to upgrade to. **Status: experimental compatibility fixture. It is not a supported production database.**

## What the fixture patch changes

`tests/fixtures/postgres-arm64/nanos.patch` and `openssl.patch` contain the PostgreSQL delta. They do not patch Jerboa or the Nanos kernel.

- The main shared-memory area is heap-backed, since there is one address space and no other process.
- Unix owner and mode checks on the data directory are disabled, because Nanos has no Unix users. Isolation comes from the VM boundary.
- **No writeback hint.** PostgreSQL's `pg_flush_data()` is an advisory "start writeback" hint. On Nanos, `sync_file_range()` returns `ENOSYS` and `msync(MS_ASYNC)` returns success without writing anything. The patch stops `pg_flush_data()` from calling either, so no hint is ever issued.
  - `PG_FLUSH_DATA_WORKS` is left undefined, so crash-recovery start no longer "pre-syncs" through the missing call. That pre-sync was the source of the old `could not flush dirty data: Function not implemented` warnings.
  - The maximum of `backend_flush_after`, `bgwriter_flush_after` and `checkpoint_flush_after` is 0. A nonzero setting is rejected rather than silently ignored.
  - Durability depends only on `fsync()`/`fdatasync()`.
- **PANIC on data-file sync failure.** This backports PostgreSQL 12's `data_sync_retry = off` behaviour. After a failed `fsync()` the kernel may have dropped the dirty pages, so retrying could report false success. The following failures now PANIC, and WAL replay rewrites the data on restart:
  - failed data-file fsync in `md.c` (`mdimmedsync`, `mdsync`, `register_dirty_segment`);
  - `fsync_fname`/`durable_rename`;
  - SLRU fsync;
  - failed close in `fd.c`.

  WAL fsync already PANICked. Other callers such as two-phase state and relmapper were not changed. The backport has not been fault-injection tested.
- **Positive 32-bit thread identities.** Backends are threads, and the fork's `MyProcPid` is a 64-bit `pthread_t`, which Nanos derives from an address. `PgBackendStatus.st_procpid`, `PGPROC.pid` and `pg_backend_pid()` are `int`, so truncation could produce zero or a negative value. A `pg_stat_activity` entry is valid only when its pid is greater than 0, so affected threads silently disappeared from the view.
  - One acceptance run lost the autovacuum launcher from the view after a crash-recovery boot.
  - `MyThreadPid()` now gives each thread a unique positive id that is never the Nanos process pid. It is used in those three places. `pthread_kill` targets still use the real `pthread_t`.
- Two build portability conflicts are avoided (`thread_stack_size` minimum and a `copy_file_range` name clash).
- OpenSSL requires TLS 1.2 or later. Backend threads use the shared SSL context under a mutex, retaining a reference before a configuration reload can replace it. The socket BIO method is initialized before accepting connections.
- Configuration reload targets the postmaster thread with `pthread_kill`, and the checked-in configuration scanner's buffer stack and accepting state are thread-local. This avoids cross-thread parser corruption when SIGHUP reaches concurrent backends.
- Private-key owner/mode checks are bypassed only when `uname` identifies Nanos, which has no Unix user isolation. The regular-file and certificate/key integrity checks remain active. Linux still checks private-key permissions.

## Build recipe

The complete, reproducible input is in `tests/fixtures/postgres-arm64`:

- `Dockerfile` builds Linux ARM64 from the pinned source commit and produces a minimal root filesystem. It also builds `amcheck`, and writes `/etc/hosts` and `/etc/nsswitch.conf` (see [Statistics and autovacuum](#statistics-and-autovacuum)).
- `nanos.patch` contains the Nanos-specific PostgreSQL changes above.
- `openssl.patch` enables the TLS compatibility and threaded-context fixes above.
- `shutdown.patch` is applied after `openssl.patch` and provides the clean shutdown on a cooperative stop (see [Clean shutdown](#clean-shutdown)).
- `unikernel.toml` starts the server. It pins these settings as command-line options, which neither `postgresql.conf` nor `ALTER SYSTEM` can override:
  - `fsync=on`
  - `synchronous_commit=on`
  - `full_page_writes=on`
  - `wal_sync_method=fdatasync`
  - `password_encryption=scram-sha-256`
  - `ssl=on`
  - `track_counts=on`
  - `autovacuum=on`
- `client.go` is a small PostgreSQL wire-protocol client with SCRAM-SHA-256 over certificate-verified TLS, used only for acceptance testing from a second guest.

`initdb` runs in the Linux ARM64 Docker build stage, not inside Nanos, because it starts helper programs. It initializes the cluster with:

- `--data-checksums`
- superuser `postgres`
- `--auth-local=scram-sha-256 --auth-host=scram-sha-256`

`pg_hba.conf` contains only `scram-sha-256` entries, with `hostssl` for all TCP connections, and the build fails if `trust`, `password` or `md5` appear. Plaintext TCP sessions are rejected.

The superuser password is a BuildKit secret. It is neither a build argument nor a layer, and only its SCRAM verifier is stored in the cluster. Supply a server certificate and matching private key as separate BuildKit secrets. Unlike the password, **the private key is deliberately installed in the packaged `/db` tree and seeded volume**. Keep those artifacts private. Secret contents do not invalidate the build cache, so pass a fresh `CLUSTER_NONCE` whenever any credentials change:

```sh
docker build --platform linux/arm64 \
  --secret id=pgpass,src=/path/to/password-file \
  --secret id=pgcert,src=/path/to/server.crt \
  --secret id=pgkey,src=/path/to/server.key \
  --build-arg CLUSTER_NONCE="$(openssl rand -hex 16)" \
  --output type=local,dest=stage tests/fixtures/postgres-arm64
```

The resulting `/db` tree is packaged and used to seed a named Jerboa volume. The test client expects a certificate with the DNS SAN `postgres.jerboa.test` and receives its trusted PEM certificate through `PGCA_BASE64`. It never disables certificate or hostname verification. Use a certificate valid for the actual server name with other clients.

## Clean shutdown

A cooperative stop works as follows:

1. `jerboa stop` asks the HVF VMM to shut down, with a 35-second timeout that forces termination when it expires.
2. The VMM presses the virtio-input power button. Nanos sends `SIGTERM` to the process and arms a 30-second shutdown timer.
3. With `shutdown.patch`, PostgreSQL performs a fast shutdown:
   - open sessions are aborted and clients receive `terminating connection due to administrator command`;
   - the checkpointer writes a shutdown checkpoint, and the statistics file is written;
   - the postmaster logs `database system is shut down` and calls `exit()`.
4. Nanos syncs storage and the VM exits. The next boot logs `database system was shut down at ...` and runs no WAL recovery.

If PostgreSQL does not finish in time:

- Nanos forces a kernel shutdown after 30 seconds, and the VMM forces termination after 35 seconds. The daemon also applies its own 40-second limit.
- The next boot then runs crash recovery.
- `jerboa stop --force` always kills the VMM immediately.

The patch is needed because the threaded fork keeps signal handlers per thread, but `pqsignal()` installs one process-wide `sigaction`. Nanos delivers a process-directed signal to an arbitrary thread, so the shutdown `SIGTERM` could run a backend's handler, or none, instead of the postmaster's. The patch makes four changes:

- A process-directed `SIGTERM`, `SIGINT` or `SIGQUIT` (`SI_USER`) is forwarded to the postmaster thread. Internal thread-to-thread signals are not forwarded.
- On Nanos only, `SIGTERM` is treated as `SIGINT` (fast shutdown). A smart shutdown would wait for clients and exceed the 30-second limit.
- The postmaster registers its thread before installing handlers.
- `proc_exit()` on the postmaster thread calls `exit()` rather than `pthread_exit()`, so no auxiliary thread keeps the VM alive.

A process-directed signal that arrives before the postmaster registers its thread keeps the old behaviour.

Validation on 2026-09-14, Apple M5, reviewed bundle, stopping by VM ID:

- **Dedicated harness** (`scripts/test-postgres-shutdown.py`, run `dist/claude-followup/shutdown/acceptance-20260914-211359`):
  - an idle stop, three stops under load and a stop after crash recovery all shut down cleanly in about 0.12–0.21 seconds;
  - a `--force` stop recovered all 1853 acknowledged commits;
  - a guest that ignores `SIGTERM` stopped at 30.08 seconds;
  - without the patch, a cooperative stop timed out at 30.085 seconds and the next boot ran crash recovery.
- **This acceptance test** (`dist/claude-followup/autovacuum/acceptance-20260914-211432`): the cooperative stop was clean in 0.311 seconds and statistics persisted.

The later integrated validation on the same M5 uses the newly built
`0.0.0-integrated-closure` bundle, including the daemon's stop-by-name fix and the
VMM storage-error latch. It stops by **name**, not the historical ID workaround:

- `qualified-postgres/acceptance-20260914-224937`: clean stop in 0.147 seconds,
  persisted statistics, and all 618/634/647 acknowledged commits recovered in
  three forced-stop rounds. SQL/protocol cancellation, permission checks,
  autovacuum lock cancellation and the guarded serial fallback all pass.
- `qualified-shutdown/acceptance-20260914-224938`: six clean stops in
  0.097-0.276 seconds; a forced kill recovers all 2235 acknowledged commits.
  The ignoring guest and the unpatched control still reach the 30-second timeout.

These paths are under `dist/integrated-closure/`. Both runs use the same final
PostgreSQL binary and fixture hashes. This remains experimental qualification,
not approval for production or physical power-loss certification.

## Statistics and autovacuum

Earlier fixtures ran with `track_counts=off`. Every boot logged `could not resolve "localhost": Name or service not known`, then `disabling statistics collector for lack of working socket` and `autovacuum not started because of misconfiguration`.

The cause was name resolution, not Nanos networking. PostgreSQL's statistics collector binds a UDP socket to the result of `getaddrinfo("localhost")`, and the package had no `/etc/hosts`, so that lookup failed.

The fixture now ships `/etc/hosts` (`127.0.0.1` and `::1` for `localhost`). A boot with only that file started the collector without warnings. The fixture also ships `/etc/nsswitch.conf` (`hosts: files dns`), which only makes the lookup order explicit. The acceptance script maps both into the package. No PostgreSQL or Nanos change was needed: the loopback UDP send, `select()` and receive self-test of the collector passes on Nanos. Both settings are pinned `on` on the command line.

The acceptance test checks that collection and autovacuum really work, rather than only that the warnings are gone:

- The autovacuum launcher appears in `pg_stat_activity`, and `pg_stat_get_snapshot_timestamp()` is fresh.
- Table counters match the statements that ran exactly: inserts, updates, deletes, live and dead tuples, and sequential and index scans. It also checks `pg_stat_database.xact_commit`.
- Eight concurrent sessions insert and delete; the totals must be exact. Backends are threads in this fork, so this checks that per-session counters are not lost or mixed.
- `autovacuum_naptime` is set to `1s` with `ALTER SYSTEM` and a reload. This setting persists in the test volume.
  - `av_probe` has per-table thresholds of 100 rows and scale factor 0.
  - Without any manual `VACUUM` or `ANALYZE`, `autovacuum_count` and `autoanalyze_count` must rise and `n_dead_tup` must reach 0.
  - `pg_class.reltuples` and `pg_stats` must be updated.
  - The server log must contain `automatic vacuum` and `automatic analyze` for the table.
- `av_control` has `autovacuum_enabled = false`. It must keep its dead tuples and show zero vacuum and analyze runs.
- After the server VM is replaced on the same volume, the collector and launcher run again and a new deletion triggers another automatic vacuum. A clean shutdown must preserve the counters.
  - After a clean shutdown the counters persist. In the combined local run, the automatic vacuum count went from 1 to 2 across the restart.
  - After crash recovery, PostgreSQL resets all statistics by design (`pgstat_reset_all`). A `jerboa stop --force`, or a cooperative stop that does not reach the guest, triggers this reset.
  - Autovacuum then starts from zero counts: a table's accumulated dead tuples are not scheduled until new changes cross its threshold.
- Every server log, including crash-recovery boots, must not contain collector or autovacuum failure messages such as `statistics collector`, `using stale statistics` or `autovacuum not started`.

## Acceptance and durability test

### Thread identities and query execution

SQL statistics and `pg_backend_pid()` use positive, non-recycled 32-bit IDs.
`PGPROC.thread_id` separately holds the real `pthread_t`. Signal delivery resolves
the public ID while holding `ProcArrayLock`, keeping the backend slot alive until
the signal has been sent. Permission checks for SQL cancellation are preserved.
`pg_cancel_backend`, `pg_terminate_backend`, protocol CancelRequest and cancellation
of a blocking autovacuum worker use this separation. BackendKeyData carries the
same ID as SQL; a wrong cancellation key or an expired ID cannot signal a backend.
Signal-mask variables are explicitly initialized for each new pthread. A
CancelRequest arrives before that connection has a PGPROC, so it retries
conditional lock acquisition without joining a wait queue; startup timeout and
termination signals are blocked only while attempting or holding the lock.

The same distinction applies to lock-group membership, serializable-transaction
wakeups and buffer-pin waiters. A separate public leader ID fixes the early
`lost connection to parallel worker` failure, but repeated parallel aggregates
still hung in this old fork. The Nanos port therefore uses PostgreSQL's existing
leader-only fallback in `CreateParallelContext`. It does not launch parallel
query workers, even if a client raises the parallel settings. Other operating
systems retain the original parallel execution path.

The package defaults `max_parallel_workers` and
`max_parallel_workers_per_gather` to zero to avoid unnecessary parallel plans.
Multiple sessions still execute concurrently, and autovacuum remains enabled.
The acceptance test deliberately overrides these settings, requires an
`EXPLAIN ANALYZE` with zero launched workers and verifies repeated aggregates.
This is a containment of the fork's executor limitation, not a repair or
qualification of parallel query execution.

The acceptance test exercises SQL cancel/terminate, permission denial, correct
and incorrect protocol cancellation keys, stale IDs, and an actual autovacuum
worker blocking an access-exclusive table lock. The server must remain usable.

### Running the suite

Run on Apple Silicon macOS with Docker available. Explicitly select an updated bundle so an old VMM is not accidentally qualified:

```sh
JERBOA_TEST_BIN="/path/to/updated-bundle/bin" \
JERBOA_TEST_GO="$PWD/../firecracker-macos/experiments/hvf/build/guest-tools/go/bin/go" \
python3 scripts/test-postgres-arm64.py
```

Optional overrides:

- `JERBOA_FIRECRACKER_BIN`: VMM override; otherwise uses `JERBOA_TEST_BIN/firecracker`. `JERBOA_TEST_BIN` is required.
- `JERBOA_TEST_EVIDENCE`: evidence directory. The default is `dist/claude-closure/postgres`.
- `JERBOA_PG_KILL_ROUNDS`: number of abrupt-stop rounds. The default is 3.
- `JERBOA_PG_NEGATIVE_CONTROL`: run the fsync-off control. The default is `1`.
- `JERBOA_PG_SUBNET`: test subnet. The default is `172.31.231`.

The test uses its own temporary `HOME`, daemon socket, network, volumes, VM names, random password and two-day self-signed certificate. The password is redacted from retained evidence; the private key, package store and seeded data trees are excluded. The temporary build artifacts themselves contain the key and must remain private.

It runs these phases and fails on any violated criterion:

1. **SQL, settings and authentication.** A separate ARM64 guest checks:
   - `fsync`, `synchronous_commit`, `full_page_writes` and `data_checksums` are all `on`;
   - `password_encryption` is `scram-sha-256` and `wal_sync_method` is `fdatasync`;
   - all `*_flush_after` settings are 0, and `SET backend_flush_after = 16` is rejected.

   It then runs CREATE/INSERT/UPDATE/DELETE, a unique-violation error with session recovery, ROLLBACK, an index scan, PL/pgSQL `DO`, VACUUM ANALYZE and CHECKPOINT. Empty and wrong passwords must fail with `password authentication failed`. The client refuses any server that accepts it without SCRAM, which catches a regression to `trust`. It verifies `pg_stat_ssl`, rejects plaintext startup, TLS below 1.2, an untrusted certificate and an incorrect server name, and opens concurrent TLS sessions during configuration reloads. It then runs the statistics and autovacuum checks described above.
2. **Cooperative stop.** The server VM is stopped normally, removed, and recreated on the same volume, and the data is read back. The server is stopped by VM ID. With `shutdown.patch`, the next boot logs `database system was shut down at` without crash recovery; the combined local run on 2026-09-14 stopped in 0.311 seconds. The statistics and autovacuum restart checks then run. `RESULT.json` records whether the stop was clean and whether the counters persisted. A clean stop requires the counters to have persisted.
3. **Abrupt stop.** A client commits one row per autocommit transaction and prints `ACK n` only after PostgreSQL acknowledges the COMMIT. It also runs `CHECKPOINT` every 250 commits. After a random number of acknowledgements, `jerboa stop --force` kills the VMM process. The killed server never sends a TCP reset over the userspace network, so the test captures the client's ACK lines and then force-stops the client too. A replacement server on the same volume then runs crash recovery. The criteria after recovery are:
   - every commit whose acknowledgement the host observed is present;
   - every payload is intact;
   - a full heap scan passes page-checksum verification;
   - index and heap counts agree;
   - `amcheck` `bt_index_check` passes on both indexes;
   - the earlier SQL-phase data is intact;
   - the server log contains no `could not flush dirty data`, `PANIC:`, `could not fsync`, `invalid page` or checksum failure;
   - the recovered server's collector and autovacuum launcher are running.

   Autovacuum, with a 1-second naptime, runs during these rounds, so a kill can land during automatic maintenance.
4. **Negative control.** The same abrupt stop runs with `fsync=off` on a separate volume. The outcome is recorded, not asserted. This shows whether the kill test can detect a durability violation at all. In the local run on 2026-09-13, the `fsync=off` cluster could not recover: startup failed with `PANIC: replication checkpoint has wrong magic 0 instead of 307747550`. The `fsync=on` rounds on the same kill path recovered every acknowledged commit.

Evidence and a `RESULT.json` with per-round numbers are retained under `dist/claude-closure/postgres/acceptance-*` on success or failure.

## Durability model and limits

The storage chain on macOS is:

1. PostgreSQL `fsync`/`fdatasync`.
2. Nanos commits page-cache writes and the TFS metadata log.
3. Nanos sends a virtio-blk `FLUSH` request.
4. The HVF VMM (`firecracker-macos/src/hvf-vmm/native/devices.c`) writes through `pwrite()` and answers `FLUSH` only after host `fcntl(F_FULLFSYNC)` succeeds. Interrupted calls are retried; unsupported operations and I/O errors are reported to the guest, without silently falling back to weaker `fsync()`.

What this does and does not cover:

- **Guest crash or VMM kill: tested.** The abrupt-stop phase covers acknowledged data reaching the host file before the VMM dies.
- **Host crash or power loss: not qualified.** The VMM now requests the stronger macOS flush. Fault-injection tests cover success, interruption, unsupported operation and I/O error, but these tests cannot establish the behaviour of physical storage during an actual power cut.
- **Storage errors fail closed.** A failed write or full flush latches an error on
  that VMM drive. Later flushes fail and snapshots are refused until the VMM is
  restarted. This prevents a later successful host flush from concealing data or
  metadata lost after an earlier error. TFS's underlying dirty-state handling on
  failed flushes is not repaired by this guard; other backends are not qualified.
- **Host-cache loss simulation: tested separately.** The journal-only VMM and
  `scripts/test-postgres-power-loss-sim.py` replay complete write buffers, keeping
  their order and dropping selected unflushed writes. Full replay must match the
  killed disk; acknowledged commits and database integrity are checked after
  recovery. Earlier flush-boundary cuts check consistency, not the ACK count at
  that earlier instant. Negative controls detect disabled and falsely successful
  flushes. Torn writes, SSD reordering and actual power loss are not modeled.

Use matching updated VMM binaries; an older bundle does not gain this behaviour by updating the fixture alone.

Other limitations:

- **TLS does not make the old fork maintained.** Keep the server on an isolated Jerboa network. The fixture validates server-authenticated TLS and SCRAM, not client-certificate authentication.
- **Build-time TLS key.** Every volume seeded from one package initially shares its certificate and private key. Use distinct credentials per deployment and rotate them before expiry. Nanos cannot enforce Unix permissions between components of the same application; protect the host artifacts and VM boundary.
- **Build-time password.** The cluster, and so the superuser verifier, is created at build time. Every volume seeded from the same package shares that password. Rotating it requires `ALTER ROLE` or rebuilding the package.
- **Environment exposure.** The test passes the client password as a guest environment variable, which is visible to anyone who can inspect the VM.
- **Unmaintained codebase.** The fork has had no upstream changes since 2018 and receives no PostgreSQL security fixes.
  - Moving to a maintained PostgreSQL release is a major project: rebasing the threading model onto current PostgreSQL, or waiting for upstream multithreading work to land.
  - It would also mean re-auditing the Nanos syscall surface used by current releases, such as `io_uring` and `sync_file_range` fallbacks.
  - Until that exists, do not store data you cannot recreate.
- **Historical bundles need replacement.** The old `local-closure` bundle does
  not include the resolved-ID fix for stopping by name or the storage-error
  latch. Updated builds use `m.vmSockPath(v.ID)` and support cooperative stop by
  name, prefix or ID. Both acceptance scripts now test names by default;
  `JERBOA_PG_STOP_BY_NAME=0` is only for controls against historical binaries.
- **No intra-query parallel workers on Nanos.** Queries use the leader-only
  fallback described above. Repairing the fork's parallel executor remains
  separate from the supported experimental configuration.
- **Statistics after crash recovery.** Counters are reset whenever the server starts with crash recovery; see [Statistics and autovacuum](#statistics-and-autovacuum).
- **Cosmetic startup messages.** Sentinel files that preserve empty cluster directories produce harmless startup messages when PostgreSQL scans `pg_tblspc` and `pg_logical/snapshots`.
- **libgcc.** `libgcc_s.so.1` is packaged explicitly because glibc loads it at runtime for pthread teardown.

Background on the port and threaded fork:

- <https://nanovms.com/dev/tutorials/running-postgres-in-a-unikernel>
- <https://github.com/postgrespro/postgresql.pthreads>
