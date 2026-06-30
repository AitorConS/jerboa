# PostgreSQL on Jerboa - persistent data with volumes

PostgreSQL needs a data directory that survives VM restarts. Jerboa volumes are
TFS-formatted disks that the guest kernel mounts at a path you choose at run
time (`-v <volume>:<guest-path>`), so the same mechanism works for every
database - no per-database engine changes.

The image (`eyberg/postgresql:11.3.0`, pulled as an `ops` package) ships the
PostgreSQL binaries, but a fresh data directory must be initialised before the
server can start. Because a newly created volume is empty too, mounting it over
the database path means you must seed the volume first with `initdb`, then run
the server against that same volume.

## 1. Create a persistent volume

```sh
jerboa volume create pgdata --size 1G
```

The volume is formatted as a labelled TFS filesystem (label = `pgdata`) the
first time it is attached. Re-attaching it never reformats, so data is
preserved.

## 2. Seed it once (initialise the data directory)

Build and run the init image from `../postgresql-init`, mounting the volume at
the data dir. `initdb` writes the PostgreSQL cluster files into the volume and
exits.

```sh
jerboa build ../postgresql-init --name postgresql-init --pkg eyberg/postgresql:11.3.0 --pkg-source ops
jerboa run postgresql-init -v pgdata:/var/lib/postgresql/data
```

The `512M` memory comes from `[run] memory` in the image's `unikernel.toml` — no
`--memory` flag needed.

## 3. Run the server against the persisted data

```sh
jerboa network create mynet
jerboa build . --name postgresql --pkg eyberg/postgresql:11.3.0 --pkg-source ops
jerboa run postgresql -v pgdata:/var/lib/postgresql/data --network mynet
```

`[run]` in `unikernel.toml` is inherited at run time: memory (`512M`) and the
published port (`5432:5432`) are applied automatically — the port publishes
because the VM joined `mynet`. Any explicit flag (`--memory`, `--cpus`, `-p`)
still overrides the baked default.

Both images declare `dirs = ["/var/lib/postgresql/data"]` under `[build]`, so the
mount point exists in the root image before the volume is attached — a volume can
only be mounted onto a directory that already exists. Build with a config in a
non-default location via `-f`/`--file`:

```sh
jerboa build . --name postgresql --pkg eyberg/postgresql:11.3.0 --pkg-source ops -f ./unikernel.toml
```

Stop and start the VM as often as you like: the data lives in `pgdata`, not in
the (ephemeral) root image. To wipe it, `jerboa volume rm pgdata`.

## Notes

- `dirs` under `[build]` bakes empty directories into the image. Volume mount
  points must be listed here; the boot-time mount silently fails if the target
  directory does not already exist in the root image.
- The `postgres` binary runs from its real package path (e.g.
  `/usr/local/postgresql/bin/postgres`), not a flattened `/program`, so it can
  locate its installation prefix (`share/`, `lib/`) and `$ORIGIN`-relative
  shared libraries such as `libpq.so.5`.
- `[run]` (memory, cpus, ports) is baked into the image and inherited by
  `jerboa run`. Explicit flags win; baked ports only publish when the VM joins a
  network (there is nothing to forward through otherwise).
- `disk_size` in `unikernel.toml` only reserves scratch space **inside the root
  image** (lost on stop). Use a volume for anything you need to keep.
- The volume mount is injected at boot: QEMU via the `opt/uni/mounts` fw_cfg
  key, Firecracker via the `mounts.<label>=<path>` kernel boot argument. The
  guest kernel matches the volume by its TFS label.
- `postgres` is configured with `listen_addresses=*` so it accepts connections
  on the managed network interface.
