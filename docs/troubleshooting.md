---
layout: default
title: Troubleshooting
description: "Real jerboa and Nanos error messages, grouped by when they happen, with the cause and the fix for each."
nav_order: 8
---

# Troubleshooting — Common Errors
{: .no_toc }

Every error on this page is real: either Jerboa prints it, or the Nanos guest
prints it on the serial console. Errors are grouped by *when* they happen —
during the build, at boot inside the guest, at run time, on Windows/WSL, or on macOS.
If a concept below is unfamiliar (one-process model, packages, program path),
read [Build Concepts]({% link build-concepts.md %}) first.

`jerboa build` runs **preflight checks** that catch many of the boot-time
errors before the image exists, and `jerboa logs` / `jerboa run --attach`
print an explanation automatically when they recognize a known guest failure —
so in many cases the fix is already on your screen.

When something is wrong and no error on this page matches, start with
`jerboa doctor`: it checks the machine, the daemon and everything the daemon
stores, and prints what to do about each problem it finds (see
[Start here: `jerboa doctor`](#start-here-jerboa-doctor)).

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

## Start here: `jerboa doctor`

```sh
jerboa doctor          # changes nothing; exits non-zero when it finds a problem
jerboa doctor --deep   # also re-reads every image disk against its digest
```

It reports on the CLI's config and credentials files, hardware virtualization,
whether the daemon answers and runs the same version as the CLI, the
hypervisor, free disk space, and the consistency of images, VMs, volumes,
networks and snapshots. Each finding comes with the command that fixes it.
When the daemon does not answer, the local checks still run and the daemon is
reported as the problem, with the command that starts it on this platform.
See the [CLI reference]({% link cli-reference.md %}#jerboa-doctor).

### The host is running out of disk space

`jerboa doctor` warns when little space is left where the daemon keeps its
data. This is the host's disk, not the guest's (for the guest, see
[`no space left on device`](#no-space-left-on-device)).

```sh
jerboa system df                # what takes the space, and how much nothing uses
jerboa system prune --dry-run   # what a prune would remove
jerboa system prune             # remove what nothing refers to any more
```

A plain `prune` only removes what you can no longer name: images a rebuild
left without a name, boot disks of VMs that are gone, build cache no image
needs, and leftovers of interrupted operations. To go further, add `--vms`
(every stopped VM), `-a` (every image no VM uses) or `--volumes` (every volume
no VM mounts; **their data is lost**). It prints the exact list and asks
before removing anything.

---

## Build-Time Errors

These come from `jerboa build` itself, before any VM exists.

### `unable to detect project language`

Jerboa looks for marker files: `go.mod` (go), `Cargo.toml` (rust),
`package.json` (node), `pyproject.toml` / `requirements.txt` (python). None
was found — or the path points at something that is neither a source project
nor an ELF binary.

**Fix**: pass `--lang <go|node|python|rust|raw>`, or set `lang` in
`unikernel.toml` (`jerboa init` writes one for you). If you meant to build a
prebuilt binary, point `jerboa build` directly at the ELF file.

### `raw build needs a program`

`lang = "raw"` means "don't compile anything — run a binary from a package",
but Jerboa could not figure out *which* binary. Raw builds resolve the program
from, in order: `[program] path` in `unikernel.toml`, then the `Program`/`Args`
declared by the ops package's own `package.manifest`.

**Fix**: add a `[program]` section with the full in-image path:

```toml
[program]
path = "/usr/local/pgsql/bin/postgres"
```

or use an ops package that declares its program (most do), and make sure the
package is actually listed (`pkgs = [...]` or `--pkg`).

### `program not found in resolved packages`

`[program] path` did not match any file shipped by the declared packages.
Matching tries the exact path, then a path suffix, then the basename.

**Fix**: list the package contents to find the real path:

```sh
jerboa pkg get eyberg/postgresql:11.3.0
# the package tree is extracted under the local package store; check the
# build's verbose output (-V) for the resolved file list
```

Then set `[program] path` to the full path as it exists **inside the image**.

### `not an ELF binary` (preflight)

The program you pointed at is not a Linux ELF — commonly a Windows `.exe`, a
macOS Mach-O binary, or a shell script. Only Linux ELF binaries can boot on
Nanos, and there is no shell in the guest to run scripts.

**Fix**: cross-compile for Linux (`GOOS=linux GOARCH=amd64`, or
`cargo build --target x86_64-unknown-linux-musl`; on a Mac use
`GOOS=linux GOARCH=arm64`), or use a package that ships a Linux build.
On macOS this error reads
[`is a macOS Mach-O executable, not a Linux ELF binary`](#is-a-macos-mach-o-executable-not-a-linux-elf-binary). If it is a launcher script, find the real binary it eventually
starts and use that.

### `32-bit ELF` / `unsupported architecture` (preflight)

Nanos images boot 64-bit x86_64 (or arm64) binaries only.

**Fix**: rebuild the program for 64-bit Linux — `x86_64`, or `arm64` on a Mac.

### `interpreter ... not found in image` / `missing shared libraries` (preflight)

The program is **dynamically linked**: it needs its dynamic loader (e.g.
`/lib64/ld-linux-x86-64.so.2`) and every shared library in its dependency
closure to be inside the image, at the paths the binary expects. Preflight
walked the binary's `DT_NEEDED` entries and could not resolve some of them
against the files being packed.

**Fix**, one of:

- add the package that ships the missing libraries (`--pkg` / `[build] pkgs`)
- build a **static** binary instead (Go with `CGO_ENABLED=0`, Rust with the
  musl target) — static binaries need nothing else
- if importing from Docker, `jerboa pkg from-docker` bundles the library
  closure automatically — prefer it over hand-copying `.so` files

Preflight errors abort the build on purpose: the same problem at boot time
produces a much more cryptic message inside the guest. If you are certain the
check is wrong, `--no-preflight` skips it.

### `pip install` fails or wheels are missing (python builds)

Python builds install dependencies as **Linux x86_64 wheels** regardless of
your host OS (the guest is Linux even when you build from Windows or macOS).
Packages that only ship source distributions (no manylinux wheel) cannot be
compiled for the guest.

**Fix**: pin a version of the dependency that publishes manylinux wheels, or
vendor a pure-Python alternative. Delete `packages/` (or
`packages/.jerboa-deps-stamp`) to force a clean dependency reinstall.

### `can't find crate` / linker errors mentioning `musl` (rust builds)

The Rust driver targets `x86_64-unknown-linux-musl` to produce a static
binary, and that target is not installed.

**Fix**:

```sh
rustup target add x86_64-unknown-linux-musl
```

### `different content collides at /<path>`

Two files that end up at the same path inside the image have different content —
for example, two packages that both ship a `README.md` at the image root.

Files from your own project always take priority over package files at the
same path, so this error only appears when two packages, or two files of your
project, claim the same path.

**Fix**: remove one of the conflicting packages, or move one of your own files
to a different path (for example with `--map source=destination` on
`pkg create`).

### `package not found in index`

The package reference does not exist in the selected source, or the source is
wrong. The default source is `ops` (`repo.ops.city`); ops refs always have a
namespace: `eyberg/postgresql:11.3.0`, not `postgresql:11.3.0`.

**Fix**: search first, then copy the exact ref:

```sh
jerboa pkg search postgres
jerboa pkg get eyberg/postgresql:11.3.0
```

Locally created packages (`pkg create`, `pkg from-docker`) live in the
`jerboa` source — build with `--pkg-source jerboa` (or `pkg_source = "jerboa"`)
to use them.

### `sets [build] sandbox = false` / `sets [build] network = true`

```
build: unikernel.toml sets [build] network = true, which lets the project's [build] run commands reach the network; rerun with --allow-build-network to allow it
```

`unikernel.toml` comes with the project, and so do the commands the build
sandbox confines, so the file cannot relax the sandbox on its own. The build
stops before running anything.

**Fix**: read the project's `[build] run` commands, then pass the flag the
message names if you trust them:

```sh
jerboa build . --allow-build-network     # for [build] network = true
jerboa build . --allow-unsandboxed       # for [build] sandbox = false
```

`examples/nextapp` needs `--allow-build-network`. See
[build-concepts.md](build-concepts.md).

### `a build step changed the build's policy files`

A confined build step wrote or created `unikernel.toml` or `.unignore`, the
files that decide how the build runs. The build fails, and a file the step
created is removed.

**Fix**: check what the project's build commands do before building it again.
A project that has to generate those files must do so before `jerboa build`.

### `ops package "<name>" not found in the eyberg namespace`

A runtime named without a namespace (`node:20`, `python:3.12`, or the one a
language build picks for you) is looked up in the `eyberg` namespace only. When
`eyberg` has no package for that version or architecture the build fails
instead of taking another publisher's package of the same name.

**Fix**: pick a version `eyberg` publishes (`jerboa pkg search node`), or name
the publisher you want:

```sh
jerboa build . --pkg <namespace>/node:20
```

### `package index serial N is older than one already accepted`

The signed package index carries a serial that grows with every publication,
and `jerboa` remembers the newest one it accepted from each index URL. An
older index, even with a valid signature, is refused and the cached copy is
kept. `package index has no serial, but serial N was already accepted` is the
same refusal for an index in the format before serials.

**Fix**: retry later; a mirror or cache may be serving a superseded index. If
the record itself is damaged (`package index state ... is corrupt`), delete
`index-state.json` beside the cached `index.json`.

---

## Boot-Time Errors (Guest Serial Output)

These appear in `jerboa logs <id>` or `jerboa run --attach` output. The CLI
recognizes all of them and prints the explanation inline — this section is the
long form.

### `popen failure: Cannot allocate memory`

Despite the wording, this is **not** an out-of-memory condition. The program
tried to launch a child process (`fork`/`exec`), and Nanos is a
single-process kernel: there is no fork, no exec, no shell. Anything that
shells out — `initdb`, Docker-style entrypoint scripts, process managers,
"worker pool via fork" designs — cannot run in a unikernel.

**Fix**: do the work at build time instead. For PostgreSQL, use the
`eyberg/postgresql` package, which ships a **pre-initialized data directory**
baked into the image, so `initdb` never needs to run. In general: use a
package that ships the pre-computed result, or configure the program to work
in-process (threads are fine; processes are not).

### `unimplemented syscall`

The program used a Linux syscall Nanos does not implement — most often process
management (`fork`, `exec`, `clone` for processes) or an exotic `ioctl`.

**Fix**: check whether the program can be configured not to spawn helpers or
daemonize (e.g. run "in foreground" modes, disable worker processes). If the
syscall is essential to the program's design, it is not unikernel-compatible.

### `could not locate my own executable path`

The program looks up its own path (`/proc/self/exe`) to find its install
prefix (`../share`, `../lib`) — PostgreSQL and MySQL both do this — but it was
executed from a different path than its install tree expects. The usual cause:
`[program] path` was a bare name (`postgres`) that matched a same-named
**launcher stub at the image root** instead of the real binary.

**Fix**: set the **full in-image path** in `unikernel.toml`:

```toml
[program]
path = "/usr/local/pgsql/bin/postgres"   # not just "postgres"
```

### `error loading shared library ...` / `cannot open shared object file`

A shared library the program needs is missing from the image (or present at
the wrong path). This is the boot-time version of the preflight "missing
shared libraries" error — if you skipped preflight with `--no-preflight`,
this is what it was protecting you from.

**Fix**: add the package that ships the library, or switch to a statically
linked binary. Rebuild and let preflight verify the closure resolves.

### `no space left on device`

The root image filesystem is full. By default the image is sized to its
contents — **there is zero free space** for logs, temp files, or database
writes.

**Fix**: reserve room at build time:

```toml
[build]
disk_size = "1G"
```

For data that must also *persist*, use a volume instead of (or in addition to)
free image space.

### `read-only file system` / writes to a missing directory fail

The program wrote to a path that does not exist in the image. Nanos cannot
create top-level directories at run time that were never packed.

**Fix**: declare every directory the program writes to:

```toml
[build]
dirs = ["/data", "/tmp"]
```

`dirs` is also required for **volume mount points** — a volume can only be
mounted onto a directory that already exists in the image.

### `out of memory` / `failed to allocate`

The guest genuinely ran out of RAM (unlike `popen failure`, which only
mentions memory). The default VM memory is 256M.

**Fix**: raise it per run or bake a default into the image:

```sh
jerboa run app:latest --memory 512M
```

```toml
[run]
memory = "512M"
```

### The VM exits immediately with no output

The program started and exited before printing anything — commonly: it
expected a config file or environment variable that is missing, it was a
launcher stub that failed silently, or it daemonized (which Nanos cannot do).

**Fix**: build with `--smoke` to catch this class at build time; run with
`--attach` to see everything from the first byte; check `[env]` against what
the program requires; make sure the program runs in **foreground** mode
(e.g. `nginx -g "daemon off;"` style flags — daemonizing forks).

---

## Run-Time Errors

### `the daemon is not answering; it may be stuck`

A command was waiting for the daemon and the daemon stopped answering at all,
as opposed to taking long over the command: the CLI checks after 10 seconds
without an answer and every 30 seconds from then on.

**Fix**: `jerboa daemon restart` on Windows and macOS,
`sudo systemctl restart jerboad` on Linux. Running VMs are recovered when the
daemon comes back. If it happens again, keep the daemon's log
(`jerboa daemon logs`, or `journalctl -u jerboad` on Linux).

### `the daemon did not answer the handshake within 15s`

Something accepted the connection and never spoke the protocol: a daemon that
is hung, or another program listening on the daemon's address.

**Fix**: check what is behind the endpoint (`jerboa daemon status` prints it)
and restart the daemon.

### `daemon busy: too many connections waiting to authenticate; retry shortly`

The daemon holds a limited number of connections that have not yet proved
they have the token: 64 in total and 8 from one peer, each with 5 seconds to
finish. A new connection from a peer that already has 8 closes the oldest of
them instead, so this message means all 64 are taken. Many commands started
at the same moment from different peers, or clients with the wrong token
retrying, fill it. `jerboa` retries a few times within about two seconds
before showing the message.

**Fix**: retry. If it persists, look for the process that keeps connecting
with a wrong or missing token (`[daemon] token` in `~/.jerboa/config.toml`, or
`JERBOA_AUTH_TOKEN`).

### `a daemon is already listening on ...`

`jerboad` was started on an endpoint that another daemon is serving. It
refuses rather than take the socket from under it.

**Fix**: use the running daemon, or stop it first. On an installed Linux
machine the service owns the endpoint: `sudo systemctl restart jerboad`.

### `record running state: ...` when starting a VM

The daemon could not save the VM's state, almost always because the disk that
holds its data is full. The VM is not left running without a record.

**Fix**: free space (`jerboa system df`, `jerboa system prune`) and start it
again.

### `image paths are accepted only from local clients`

`jerboa run /path/to/disk.img`, or a compose `image:` that is a file path,
reached a daemon over TCP. That is the normal connection on Windows and on a
Linux machine set up with the installer. The daemon boots a disk file only
for a caller it can identify, through its Unix socket, and only a file that
caller owns.

**Fix**: build or pull the image and run it by name
(`jerboa build . --name app`, then `jerboa run app:latest`).

### `--port requires --network`

On Linux and Windows, port publishing goes through the managed network's
bridge (kernel NAT or the userspace proxy, see `jerboad --port-publish` in the
CLI reference); there is no SLIRP fallback. (On macOS, `-p` works without
`--network`.)

**Fix**:

```sh
jerboa network create app
jerboa run web:latest --network app -p 8080:80
```

### UDP port mapping does not forward

On Linux, UDP ports are published by kernel NAT, the default where nftables is
available (`jerboad --port-publish auto`, `nat` or `nat-lan`). With
`--port-publish proxy`, on WSL2, or when kernel NAT is unavailable, the
userspace proxy skips UDP mappings with a warning in the daemon log. macOS
forwards both TCP and UDP.

### Published port unreachable from other hosts with `--port-publish nat-lan`

jerboad accepts the forwarded connections in iptables and in the forward
chains of nftables tables it finds when ports are published. A firewall
reload (for example `firewall-cmd --reload`) removes those rules until the
next port is published or the daemon restarts. Check that they are present:

```sh
sudo iptables -S | grep 'jerboa published ports'
sudo nft list ruleset | grep 'jerboa published ports'
```

Restart the daemon to restore them, or switch back to
`--port-publish auto`, where other hosts use the proxy and the host's INPUT
rules apply.

### Guest DNS fails on Linux while connections by IP work

The guest resolver sends UDP queries to `10.53.0.53:53`, an address assigned
to the host's loopback interface. Check the daemon journal first:

```sh
sudo journalctl -u jerboad --since '10 minutes ago'
systemctl show jerboad -p AmbientCapabilities -p CapabilityBoundingSet
```

If the bind fails with `permission denied`, the service needs
`CAP_NET_BIND_SERVICE` in both capability lists, alongside `CAP_NET_ADMIN`
and `CAP_NET_RAW`. Older published installers omitted that capability. Update
the service definition from the current installer and restart the daemon when
its guests can be stopped.

A successful bind does not prove the host firewall permits guest queries.
Check `sudo journalctl -k` for blocked UDP traffic to `10.53.0.53`, and use
`jerboa network inspect <name>` to find the network's `bridge` field. For
example, if UFW blocks queries arriving on `jerboa-br-app`, permit only DNS
from that bridge:

```sh
sudo ufw allow in on jerboa-br-app to 10.53.0.53 port 53 proto udp
```

Use the actual bridge name from inspection and repeat a guest lookup. This is
host-input traffic, so a forwarding rule alone does not allow it. Remove the
corresponding rule when the network is removed:

```sh
sudo ufw delete allow in on jerboa-br-app to 10.53.0.53 port 53 proto udp
```

### `pre-existing shared memory block ... is still in use` (PostgreSQL)

PostgreSQL found a stale `postmaster.pid` on its volume. It records the guest
PID of the previous postmaster, and because Nanos always runs the application
as PID 2, the entry always looks like a live process, so PostgreSQL refuses to
start.

The lock is left behind whenever the guest is not told to shut down:

- **QEMU** (Linux and macOS), **Firecracker** (Linux) and **Spark** (macOS) deliver a
  shutdown to the guest, so `jerboa stop` lets PostgreSQL checkpoint and clear
  its lock. A `stop --force`, a `kill` or a host reboot still leaves the lock
  behind.
- **Firecracker on Linux** delivers the shutdown over the VM's vsock device,
  which needs a guest kernel that handles the request. With an older kernel
  every stop is ungraceful and the lock is always left behind.

**Your data is safe.** Volumes forward the guest's flushes to host storage, so
PostgreSQL's crash recovery replays the write-ahead log on the next start,
including transactions committed after the last checkpoint.

**Fix**: remove `postmaster.pid` from the volume, then start the VM again. Do
this only when no other VM has that volume mounted — deleting the lock of a
live cluster is how a cluster gets corrupted. Re-seeding the volume also
clears it, but `jerboa volume seed` resets the data to the seed state, so
reach for it only when you want to discard the database.

### The database is empty after mounting a volume

A fresh volume is empty, and mounting an empty volume over a path that has
baked data (e.g. the pre-initialized database at `/db`) **shadows** that data.

**Fix**: seed the volume before first use — or create and seed in one step:

```sh
jerboa volume create pgdata --size 1G \
  --seed-pkg eyberg/postgresql:11.3.0 --src /db
jerboa run postgresql -v pgdata:/db --network pgnet -p 5432:5432
```

### `mount point does not exist`

Volumes mount onto directories that must already exist inside the image.

**Fix**: add the path to `[build] dirs` and rebuild:

```toml
[build]
dirs = ["/db"]
```

### `state file ... is not written by compose up for this project`

Compose keeps a stack's state in `~/.jerboa/compose/<hash>.json`. A
`.jerboa-compose-<hash>.json` left next to the compose file by an earlier
version is taken over only if it is yours, records this compose file, and
names only this project's VMs, networks and volumes. Any other such file is
refused, because it would tell `compose down` what to stop and remove, and
`up`, `down`, `ps` and `logs` do nothing until it is gone. The message ends
with the reason (owned by another user, writable by others, a VM that is not
one of the project's services, ...).

**Fix**: if the file came with a project you downloaded, delete it. If it is
your own stack from an earlier version, fix what the message names (usually
`chmod go-w` on the file) and run the command again. See
[compose.md](compose.md).

### Compose rejects a service name

Service names become VM names, so they follow the rules of network and volume
names: letters, digits and `_.-`, starting with a letter or digit. Names with
spaces or other characters that earlier versions accepted are now refused.

**Fix**: rename the service in the compose file. Bring a stack started under
the old name down with the old file first.

### jerboad says the cluster or observability token is too short

The daemon refuses a cluster token shorter than 32 characters, and an
observability token shorter than 32 characters when metrics or the dashboard
listen off loopback, because anyone who reaches the port can try tokens.
Earlier versions accepted them.

**Fix**: generate a token with `openssl rand -hex 32` and put it where the
daemon reads it: the `--cluster-token-file` of every node (or
`JERBOA_CLUSTER_TOKEN`) and restart all of them, or
`--observability-token-file` / `JERBOA_OBSERVABILITY_TOKEN`. For a daemon
started by `jerboa daemon start`, set `observability_token` in
`~/.jerboa/config.toml`, or delete it and a new one is generated. Update your
Prometheus scrape configuration with the new token. Binding metrics and the
dashboard to `127.0.0.1` also lifts the observability check.

### The dashboard asks for the token again after an upgrade

The dashboard's cookie no longer holds the observability token; it holds a
session that opens only that dashboard, for 12 hours or until the daemon
restarts. Cookies set by earlier versions are not accepted.

**Fix**: open `/ui/login` and enter the token once. `/metrics` does not take
the cookie: send the token as `Authorization: Bearer <token>`.

The session is tied to the address the browser signed in from, so the
dashboard also asks again after your machine's address changes (another
network, a VPN). A request from an address inside a jerboa network's subnet
gets 403: open the dashboard from the host or from another machine, not from
a guest.

### Console or log output loses colours or cursor movement

`jerboa logs`, `compose logs`, `pkg load` and attach output remove terminal
control sequences from guest output, so a guest cannot move the cursor, retitle
the window or write to the clipboard of your terminal.

**Fix**: pass `--raw` to print the output unfiltered when you trust the guest.

### `guest network isolation is not available, so VMs on networks do not start` (Linux)

A VM on a network starts only while the daemon's nftables rules that isolate
guests are loaded (see
[Network isolation]({% link getting-started.md %}#network-isolation-linux)).
The message carries why they are not: usually `nft not found`, on a host set
up before the installer added the `nftables` package, or an `nft` error when
the kernel or another firewall refuses the `jerboa_guard` tables.

**Fix**: install nftables and run the VM again; the daemon loads the rules on
its own:

```sh
sudo apt-get install nftables
jerboa doctor            # network isolation: guest network guard loaded
```

Running the installer again also installs it. If `nft` is present, the error
names what it refused. To run guests without isolation instead (not
recommended: a guest can then spoof other guests and reach the host's
services), set `JERBOA_ALLOW_UNGUARDED_NETWORK=1` for the daemon:

```sh
sudo systemctl edit jerboad     # add: [Service]
                                #      Environment=JERBOA_ALLOW_UNGUARDED_NETWORK=1
sudo systemctl restart jerboad
```

### A guest cannot reach a service on the host or the LAN (Linux)

Connections time out from inside the guest to an address of the host (its
gateway included), to the LAN, a VPN, a Docker network or `169.254.169.254`,
while public addresses and other guests of its network answer. That is the
default since the guest network guard keeps guests off the host and off
private networks (see
[Network isolation]({% link getting-started.md %}#network-isolation-linux)).

**Fix**: reach other guests by their address or name on a shared network
instead of through a port published on the host. If the guest must reach the
host or private networks, start the daemon with
`JERBOA_GUEST_HOST_ACCESS=1`:

```sh
sudo systemctl edit jerboad     # add: [Service]
                                #      Environment=JERBOA_GUEST_HOST_ACCESS=1
sudo systemctl restart jerboad
```

### Published ports stop answering on `127.0.0.1` (Linux)

The daemon log shows `guard: not loaded; route_localnet turned off on jerboa
bridges`. Kernel NAT for published ports needs `route_localnet` on the bridge,
which is only safe while the daemon's nftables rule that keeps guests away
from the host's loopback is loaded. When a firewall reload removed the tables
and they cannot be reinstalled, the daemon turns `route_localnet` off rather
than leave guests a path to `127.0.0.1`.

**Fix**: find why `nft` fails (the log line carries its error; a missing `nft`
binary or a ruleset that rejects the `jerboa` tables are the usual causes).
The daemon rechecks every few seconds and restores the ports on its own
(`guard: route_localnet restored`).

### `release: stable manifest generated at ... is older than ... allows`

Every client has a floor for the stable channel: the generation time of the
newest manifest it accepted, the one `install.sh` recorded in
`/etc/jerboa/release-state.json`, and a date compiled into the binary. A
manifest older than that is refused, so an old release cannot be served again
as the current one.

**Fix**: retry later; a mirror or cache may be serving a superseded manifest.
If you run a mirror, sync `channels/stable.json` and its signature.

### `config: ... is not a private directory owned by this user`

Without `HOME`, `jerboa` keeps its settings in a per-user fallback directory
and uses it only if the directory, and the files in it, belong to you and
nobody else can write them. It never falls back to the working directory.

**Fix**: set `HOME`, or remove the directory the message names if you did not
create it.

---

---

## Windows / WSL Errors

### `daemon not installed` / daemon-backed commands fail

On Windows the daemon runs inside a dedicated WSL2 distro named `jerboa`.

**Fix**:

```powershell
jerboa daemon install
jerboa daemon start
jerboa daemon status
```

Daemon-backed commands auto-start the daemon when needed; purely local
commands (`init`, `pkg list`, …) never touch it.

### Connection refused / talks to the wrong daemon on port 7890

The Windows client dials `tcp://127.0.0.1:7890` by default. If **another**
process (for example a leftover `jerboad` installed manually in a different
WSL distro) is listening on that port, the CLI reaches the wrong daemon and
sees none of your images or VMs.

**Fix**: find and stop the stray daemon inside other distros
(`wsl -d <distro> -- pkill jerboad`), or point the client explicitly at the
right endpoint with `-H`/`JERBOA_HOST`. `jerboa daemon status` shows the
endpoint the managed distro actually uses.

### Firecracker fails inside WSL

Firecracker requires KVM (`/dev/kvm`) inside the WSL2 distro, which needs
nested virtualization support on the host.

**Fix**: use QEMU, which falls back to TCG emulation without KVM, or enable
nested virtualization for WSL2. With `hypervisor` left at `auto`, the default,
the daemon picks QEMU by itself on a host that cannot run Firecracker; this
error means Firecracker was selected explicitly
(`jerboa config set hypervisor auto`, then `jerboa daemon restart`).

---

## macOS Errors

See [Using Jerboa on macOS]({% link macos-guide.md %}) for how the Mac differs
from Linux.

### `"Jerboa Desktop" Not Opened`

The Jerboa Desktop preview is not signed or notarized by Apple yet, so
Gatekeeper blocks its first launch.

**Fix**: click **Done**, open **System Settings → Privacy & Security**, click
**Open Anyway** next to the Jerboa Desktop message, confirm with your password
and open the app again.

### `Move Jerboa Desktop to the Applications folder and reopen it before starting the engine`

The engine runs from inside the app bundle, so the app refuses to start it from
the DMG or from a temporary location.

**Fix**: quit the app, drag **Jerboa Desktop** into **Applications** (or accept
**Move to Applications** when the app asks) and open it from there.

### `is a macOS Mach-O executable, not a Linux ELF binary`

You passed a program compiled for macOS. Unikernels run Linux programs only.

**Fix**: point `jerboa build` at the source directory so Jerboa cross-compiles
it, or build a Linux ARM64 binary yourself:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o myapp .
jerboa build ./myapp --name myapp
```

### `spark cannot emulate x86 images`

The image is x86_64 (or you passed `--emulate-x86`), and Spark can only
run ARM64 images.

**Fix**: rebuild the image with `--platform linux/arm64`. If you need the x86
version, switch the daemon to QEMU:

```sh
brew install qemu
jerboa config set hypervisor qemu
jerboa daemon restart
jerboa run myimage:latest --emulate-x86
```

### `native qemu executable missing` / `native spark executable missing`

The daemon is configured for an engine that is not installed. This usually
means `hypervisor` was set to `qemu` but QEMU is not installed. The default,
`auto`, uses Spark on a Mac that has it.

**Fix**: select Spark, which ships with the app, or install QEMU:

```sh
jerboa config set hypervisor auto   # or: brew install qemu
jerboa daemon start
```

### `native daemon did not start`

The daemon failed during start-up.

**Fix**: read `~/.jerboa/jerboad.log`. The daemon runs in your user session, so
start it from a logged-in desktop session (not from a plain SSH login).

### `native daemon lifecycle requires the default local socket`

`jerboa daemon start/stop/restart` on macOS only manages the local daemon, but
`JERBOA_HOST` or `daemon.endpoint` points somewhere else.

**Fix**: `unset JERBOA_HOST` (and remove `daemon.endpoint` from the config) to
manage the local daemon. To talk to a remote daemon, use regular commands with
`--host` instead.

### A VM cannot reach the Mac, the local network or the internet (Spark)

By default a Spark VM reaches the public internet and resolves external
DNS names, but not the Mac itself nor private, shared and link-local
addresses: a connection to your router, to another machine of your network or
to a service on the Mac through the VM's gateway is refused. If the internet
is unreachable too, the daemon was started with a policy file
(`JERBOA_SPARK_SECURITY` or `jerboad --spark-security`), which replaces
the default entirely. Traffic between VMs on a network and published ports
always work.

**Fix**: allow the exact destinations in a policy file and restart the daemon
with it, or start it without one to go back to the default. See
[Restricting or widening outbound access]({% link macos-guide.md %}#restricting-or-widening-outbound-access-spark).

### Cannot connect to the VM's IP from the Mac

On macOS, VM addresses live inside Jerboa's own network stack and are not
routable from the Mac.

**Fix**: publish the port and connect to `localhost`:

```sh
jerboa run web:latest -p 127.0.0.1:8080:80
curl http://127.0.0.1:8080
```

### `UNSUPPORTED_CAPABILITY` on `jerboa snapshot`

Snapshots require the Spark engine, a VM on a named network (`--network`)
and no volumes.

**Fix**: switch to Spark, run the VM with `--network <name>` and without
`-v`, then create the snapshot. Restore only works on the same VM, while it is
stopped.

### `vm lifecycle operation in progress`

Another operation (start, stop, restart, snapshot or restore) is still running
on the same VM.

**Fix**: wait for it to finish and try again.

---

## Still Stuck?

- `jerboa doctor` checks the installation and the daemon's data, and
  `jerboa doctor --output json` gives the same findings in a form to attach to
  a report.
- Re-run the build with `-V/--verbose` to see the raw driver and packaging
  output, and read the preflight report.
- `jerboa build . --name app --smoke` boots the image once right after
  building and reports known failure signatures automatically.
- `jerboa inspect <id>` dumps the full VM state as JSON.
- [Build Concepts]({% link build-concepts.md %}) explains the model behind
  most of these errors.
