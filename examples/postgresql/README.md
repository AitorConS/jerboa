# PostgreSQL on Jerboa

This builds a unikernel that boots PostgreSQL and accepts connections on port
5432, using the `eyberg/postgresql:11.3.0` package pulled from the `ops`
ecosystem.

## Why there is no `initdb` step

A PostgreSQL data directory normally has to be initialized with `initdb` before
the server can start. `initdb` **cannot run inside a unikernel**: it fork/execs
the `postgres` bootstrap process (and other helper binaries), and nanos is a
single-process kernel with no `fork`/`exec`. Attempting it fails with
`popen failure: Cannot allocate memory`.

The `eyberg/postgresql` package sidesteps this by **shipping a pre-initialized
cluster** inside the package (`sysroot/db`, containing `PG_VERSION`, `base/`,
`global/`, …). It is baked into the image at `/db`, so the server can start
against it directly — exactly what the upstream ops package manifest does
(`Args: ["/usr/local/pgsql/bin/postgres", "-D", "db"]`).

## Build and run

```sh
jerboa network create pgnet
jerboa build . --name postgresql --pkg eyberg/postgresql:11.3.0 --pkg-source ops
jerboa run postgresql --network pgnet -p 5432:5432
jerboa logs <id>     # → "database system is ready to accept connections"
```

`memory` (`512M`), `cpus`, and the published port (`5432:5432`) come from
`[run]` in `unikernel.toml` and are inherited automatically; the port publishes
because the VM joined `pgnet`. Any explicit flag (`--memory`, `--cpus`, `-p`)
still overrides the baked default.

## How the `unikernel.toml` works

- `lang = "raw"` — no compilation; the binaries come from the `--pkg` package.
- `[program] path = "/usr/local/pgsql/bin/postgres"` — the full in-image path,
  so the binary runs from its real install location and can locate its prefix
  (`../share`, `../lib`) via `/proc/self/exe` and resolve `$ORIGIN`-relative
  shared libraries such as `libpq.so.5`. Pointing at a bare `postgres` would
  instead match a same-named launcher stub at the image root (`/postgres`) and
  fail with *"could not locate my own executable path"*.
- `[program] args = ["-D", "/db", ...]` — run against the pre-seeded cluster.
- `disk_size = "1G"` — reserves scratch space inside the root image for the
  database files and runtime writes (WAL, temp, sockets).

## Persistence (seeded volume)

`/db` baked into the image is **ephemeral** — removing the VM (`jerboa rm`)
discards the data. To make the database durable, put the cluster on a TFS
volume. A freshly created volume is empty, and mounting an empty volume over
`/db` would just shadow the seeded data — so the volume has to be **seeded once**
with the initialized cluster before it is mounted. `jerboa volume seed` does
this: it writes a package subtree into the volume's filesystem with `mkfs`.

```sh
# 1. Create the volume
jerboa volume create pgdata --size 1G

# 2. Seed it with the package's pre-initialized cluster (/db → volume root)
jerboa volume seed pgdata --pkg eyberg/postgresql:11.3.0 --pkg-source ops --src /db

# 3. Run with the volume mounted at /db (it shadows the baked copy)
jerboa network create pgnet
jerboa run postgresql -v pgdata:/db --network pgnet -p 5432:5432
```

Now the data survives recreating the VM:

```sh
jerboa stop <id>          # graceful: postgres checkpoints and clears its lock
jerboa rm <id>            # remove the VM entirely
jerboa run postgresql -v pgdata:/db --network pgnet -p 5432:5432
jerboa logs <id>          # → "database system was shut down at <your last stop>"
```

The shutdown timestamp in the new VM's log matches when you stopped the previous
one (not the package's original `2023-…` build time), confirming the writes
persisted on the volume across `jerboa rm`. Wipe the data with
`jerboa volume rm pgdata` (and re-seed to start fresh).

> **Always stop with `jerboa stop`** (graceful). Postgres then checkpoints and
> removes `postmaster.pid`. An ungraceful kill leaves a stale `postmaster.pid`
> on the volume, and the next boot refuses to start
> (*"pre-existing shared memory block … is still in use"*); re-seeding the volume
> clears it.

## Notes

- `postgres` is configured with `-c listen_addresses=*` so it accepts
  connections on the managed-network interface, not just the Unix socket.
- The `could not resolve "localhost"` warning and the disabled statistics
  collector are benign: the stats collector wants a `localhost` UDP socket and
  the guest has no resolver entry for it. The server still accepts connections.
