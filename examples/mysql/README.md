# MySQL on Jerboa — persistent data with volumes

MySQL needs a data directory that survives VM restarts. Jerboa volumes are
TFS-formatted disks that the guest kernel mounts at a path you choose at run
time (`-v <volume>:<guest-path>`), so the same mechanism works for every
database — no per-database engine changes.

The image (`eyberg/mysql:5.7.29`, pulled as an `ops` package) ships a binary but
an **empty** data directory: `mysqld` must initialise it once before it can
serve. Because a freshly created volume is empty too, mounting it over
`/var/lib/mysql` would shadow anything baked into the image — so you seed the
volume first with a one-shot init image, then run the server against it.

## 1. Create a persistent volume

```sh
jerboa volume create mysqldata --size 512M
```

The volume is formatted as a labelled TFS filesystem (label = `mysqldata`) the
first time it is attached. Re-attaching it never reformats, so data is
preserved.

## 2. Seed it once (initialise the data directory)

Build and run the init image (`unikernel.init.toml`), mounting the volume at the
data dir. `mysqld --initialize-insecure` writes the system tables **into the
volume** and exits.

```sh
jerboa build . -f unikernel.init.toml -t mysql-init
jerboa run mysql-init -v mysqldata:/var/lib/mysql
```

## 3. Run the server against the persisted data

```sh
jerboa build . -t mysql
jerboa run mysql -v mysqldata:/var/lib/mysql --network mynet --port 3306:3306
```

Stop and start the VM as often as you like: the data lives in `mysqldata`, not
in the (ephemeral) root image. To wipe it, `jerboa volume rm mysqldata`.

## Notes

- `disk_size` in `unikernel.toml` only reserves scratch space **inside the root
  image** (lost on stop). Use a volume for anything you need to keep.
- The volume mount is injected at boot: QEMU via the `opt/uni/mounts` fw_cfg
  key, Firecracker via the `mounts.<label>=<path>` kernel boot argument. The
  guest kernel matches the volume by its TFS label.
