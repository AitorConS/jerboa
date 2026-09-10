---
layout: default
title: CLI Reference
nav_order: 4
---

# CLI Reference
{: .no_toc }

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

## Global Flags

Every `jerboa` command accepts:

| Flag | Description |
|---|---|
| `-H, --host` | Daemon endpoint override |
| `--store` | Local client-side store root used by commands that touch client-owned state |
| `--output table\|json` | Output format |
| `-V, --verbose` | Show raw build/download output |
| `-v, --version` | Show CLI version |

Endpoint resolution order:

1. `--host`
2. deprecated `--socket`
3. `JERBOA_HOST`
4. `[daemon] endpoint` in `~/.jerboa/config.toml`
5. platform default

Defaults:

- Linux: `unix:///var/run/jerboad.sock`
- Windows: `tcp://127.0.0.1:7890` as the configured default, but the client resolves and dials the dedicated WSL2 distro IP when auto-booting the daemon

Authentication token resolution:

1. `JERBOA_AUTH_TOKEN`
2. `[daemon] token` in `~/.jerboa/config.toml`

---

## Core VM Commands

### `jerboa run <image>`

Create and start a VM from:

- an image reference like `hello:latest`
- a direct disk image path on the daemon filesystem

Key flags:

| Flag | Description |
|---|---|
| `--memory` | VM memory, default `256M` |
| `--cpus` | vCPU count, default `1` |
| `-p, --port` | Port mapping `[bindaddr:]host:guest[/tcp\|udp]`; without a bind address the port publishes on all interfaces, `127.0.0.1:host:guest` restricts it to localhost |
| `-e, --env` | Repeatable environment variable |
| `--env-file` | Read env vars from file; explicit `-e` flags override matching keys |
| `--name` | Human-readable VM name |
| `--rm` | Auto-remove after stop |
| `-v, --volume` | Named volume mount `name:path[:ro]` |
| `--attach` | Stream serial console and block |
| `-d, --detach` | Detached mode, default `true` |
| `--network` | Managed network name |
| `--ip` | Static guest IP, requires `--network`; omitted, the daemon allocates one from the network's subnet |
| `--health-check` | `tcp:PORT` or `http:PORT:/path` |
| `--restart` | `never`, `on-failure`, `always[:max-retries]` |
| `--verify` | Signature verification mode: `off`, `warn`, `enforce` |
| `--cpu-shares` | cgroup v2 CPU weight |
| `--memory-max` | cgroup v2 memory hard limit |
| `--disk-iops` | Boot-disk IOPS throttle |
| `--disk-bps` | Boot-disk throughput throttle |

Notes:

- Port publishing requires `--network`.
- Every VM on a managed network gets a guest IP: `--ip` pins it, otherwise the daemon's IPAM allocates the next free address from the network's subnet.
- TCP forwarding works today.
- UDP mappings are currently skipped by the userspace forwarder with a warning.
- On Windows the published port lives inside the `jerboa` WSL2 distro. With
  WSL2's default NAT networking it is reachable at the distro IP (the host from
  `jerboa daemon status`), not at `localhost` on the Windows host — set
  `networkingMode=mirrored` in `%USERPROFILE%\.wslconfig` for Docker-Desktop-style
  `localhost` publishing. See [Getting Started]({% link getting-started.md %}).

### `jerboa ps`

List VMs known to the daemon. The command help still says "running", but the implementation prints the daemon registry, including stopped entries when present.

### `jerboa status`

Summary view of daemon-side VM counts and restart/health state.

### `jerboa logs <id>`

Print buffered serial console output.

Flag:

- `-f, --follow`: poll and stream appended output until the VM stops

When the output contains a known Nanos failure signature (`popen failure`,
`error loading shared library`, `no space left on device`, …), an explanation
of the cause and the fix is printed after the logs. The same detection runs on
`jerboa run --attach` output. See
[Troubleshooting]({% link troubleshooting.md %}) for the full catalogue.

### `jerboa inspect <id>`

Return full VM detail as JSON.

### `jerboa stats <id>`

Show runtime stats.

Flags:

- `-w, --watch`
- `-i, --interval`

### `jerboa stop <id>`

Graceful stop by default. Idempotent: stopping a VM that is already stopped
(for example a program that ran to completion and exited on its own) is a no-op
that succeeds, matching `docker stop`.

Flag:

- `--force`: immediate kill

### `jerboa rm <id>`

Remove a stopped VM from the daemon registry.

### `jerboa exec <id>`

Send a signal through the daemon.

Flag:

- `--signal`, default `SIGTERM`

---

## Image Commands

### `jerboa build <path>`

Build an image from a static ELF or a source directory.

Key flags:

| Flag | Description |
|---|---|
| `--name` | Image name |
| `--tag` | Image tag, default `latest` |
| `--memory` | Default memory baked into the image |
| `--cpus` | Default CPU count baked into the image |
| `--pkg` | Include runtime package(s); appends to `[build] pkgs` from `unikernel.toml` |
| `--pkg-source` | `ops` (default) or `jerboa`; an explicit flag overrides `[build] pkg_source` |
| `--lang` | `go`, `node`, `python`, `rust`, `raw` |
| `--platform` | Cross-build target |
| `--port` | Declared service port; emits the guest network section in the build manifest |
| `-f, --file` | Path to the `unikernel.toml` to use (default: `<path>/unikernel.toml`) |
| `--no-preflight` | Skip the pre-build binary checks (ELF class/arch, shared-library closure, entrypoint presence) |
| `--smoke` | Boot the image once after building, scan serial output for known failure signatures, then stop and remove the test VM |

`unikernel.toml` is read automatically when present, or selected explicitly with
`-f/--file`. Relevant `[build]` keys:

| Key | Description |
|---|---|
| `lang` | Build driver: `go`, `node`, `python`, `rust`, `raw` |
| `entrypoint` | Override the default entrypoint |
| `run` | Shell commands to run before packaging (like a Dockerfile `RUN`) |
| `pkgs` | Packages to include (e.g. `["eyberg/postgresql:11.3.0"]`); `--pkg` flags append to this list |
| `pkg_source` | Package source: `ops` (default) or `jerboa`; an explicit `--pkg-source` flag wins |
| `disk_size` | Minimum image size (e.g. `512M`, `1G`) for runtime scratch space |
| `dirs` | Absolute directories to create empty in the image — volume mount points (a volume can only mount onto a directory that already exists in the image) and runtime scratch paths |

For `lang = "raw"`, `[program] path` names the runtime binary resolved from
package files; the program is executed from its real in-image path, so binaries
that locate their installation prefix relative to their own executable (e.g.
`postgres`, `mysqld`) and binaries with `$ORIGIN`-relative library paths resolve
correctly. When `[program]` is omitted entirely, the program and its arguments
are inherited from the ops package's own `package.manifest` (`Program`/`Args`),
so well-formed ops packages build with just `pkgs = [...]`.

Before assembling the image the build runs **preflight checks**: the program
must be a 64-bit Linux ELF for a supported architecture; if dynamically
linked, its interpreter and the full recursive `DT_NEEDED` shared-library
closure must resolve against the image contents; node/python entrypoints must
be among the packed files. Errors abort the build with a fix hint;
`--no-preflight` skips the checks. See
[Build Concepts]({% link build-concepts.md %}) for the background.

`[env]` keys are baked into the image environment (highest priority — they
override env supplied by packages or the language driver).

`[run]` keys are baked into the image manifest and inherited by `jerboa run`
unless overridden by a flag (precedence: run flag > `[run]` value > built-in
default):

| Key | Description |
|---|---|
| `memory` | Default VM memory (e.g. `512M`); applied unless `--memory` is given |
| `cpus` | Default vCPU count; applied unless `--cpus` is given |
| `ports` | `host:guest` publish maps; applied when the VM joins a network (`--network`) and no `-p` is given |

`[[stages]]` defines multi-stage builds: each stage has its own `lang`,
`entrypoint`, `args`, and `copy_from` (`stage`, `src`, `dst`) directives; the
final stage's output becomes the image binary.

Build driver behavior to know:

- Go builds add `-trimpath` and `-ldflags "-s -w"` by default to reduce guest
  binary and image size. If custom build args include `-ldflags`, those args are
  appended after the defaults and take precedence.
- Node builds reinstall production dependencies when manifests, lockfiles, runtime
  version or host platform change, or when `node_modules` is missing. If `package-lock.json` exists, the driver uses
  `npm ci --omit=dev --no-audit --no-fund`; otherwise it uses
  `npm install --omit=dev --no-audit --no-fund`.
- Python builds cache dependency installs. `pip install` runs only when
  `requirements.txt` or the Python version changes, tracked by
  `packages/.jerboa-deps-stamp`. Delete `packages/` or that stamp file to force
  a dependency reinstall. Changed dependencies are installed into a clean staging
  directory, then replace the old set only after installation succeeds. Removed
  requirements therefore disappear from the rebuilt image.

Newly built images reserve a 4 MiB embedded boot filesystem partition instead
of the previous 12 MiB layout, so they are about 8 MiB smaller (for example,
`hello` drops from about 16.6 MiB to about 8.1 MiB). Older images are unchanged;
rebuild an image to get the smaller layout.

### `jerboa init [path]`

Write a commented `unikernel.toml` scaffold into `path` (default `.`). The
language is auto-detected from marker files (`go.mod`, `Cargo.toml`,
`package.json`, `pyproject.toml`/`requirements.txt`); ambiguous or empty
directories get the generic template. The generated file documents every field
inline, including the raw-mode program-path pitfalls.

Flags:

| Flag | Description |
|---|---|
| `--lang` | Force the template: `go`, `node`, `python`, `rust`, `raw` |
| `--force` | Overwrite an existing `unikernel.toml` |

`init` is a purely local command — it never contacts the daemon.

### `jerboa images`

List images stored by the daemon.

### `jerboa rmi <ref>`

Remove an image from the daemon store. Refused with an error if any VM (running
or stopped) still references the image; remove those VMs first. This mirrors
`docker rmi` and prevents leaving a VM pointing at a disk that no longer exists.
Removing by digest removes all tags pointing to that image. Ambiguous digest
prefixes are rejected.

### `jerboa sign <image>`

Resolve the image through the daemon and sign its disk digest with the default Ed25519 key.

### `jerboa verify <image>`

Resolve the image through the daemon, hash its actual disk contents, and verify the
signature against that digest. `run --verify enforce` pins the verified digest;
each hypervisor verifies the bytes while writing its private boot copy and starts
the VM only if the digest matches.

---

## Package Commands

`jerboa pkg` manages runtime packages used during builds.

Subcommands:

| Command | Purpose |
|---|---|
| `pkg list` | List locally cached packages; with no `--source` it lists **both** the ops and jerboa sources (so `pkg create` packages are visible), `--source ops\|jerboa` filters to one |
| `pkg search <query>` | Search remote index |
| `pkg get <ref>` | Download package |
| `pkg remove <ref>` | Remove cached package |
| `pkg create <name>[:version] <binary>` | Create a local package |
| `pkg from-docker <name>[:version] <image>` | Extract a binary and libraries from a Docker image |
| `pkg push <name>:<version> <index-url>` | Push a local package to a remote index |
| `pkg load <package>` | Download a package, build an image from it, and run it in one step (`-d/--detach` runs in the background) |

Supported package sources (`--source`, default `ops`):

- `ops` — the [nanovms/ops](https://ops.city) ecosystem at `repo.ops.city`;
  refs are `<namespace>/<name>:<version>` (e.g. `eyberg/postgresql:11.3.0`)
- `jerboa` — the first-party index; also where `pkg create` and
  `pkg from-docker` store their local packages

`pkg from-docker` flags:

- `--file <path>` — the binary inside the image to package. **Optional**: when
  omitted, the program is derived from the image's `Entrypoint`/`Cmd` and
  resolved on the container's `PATH`. The shared-library closure is read from the
  image's exported filesystem (no `ldd`/`cat` inside the image), so it works on
  `scratch` and distroless images too, and every file is stored at its real
  absolute in-image path. Images whose entrypoint is a shell script
  (`docker-entrypoint.sh` and friends) cannot be derived — there is no shell in a
  unikernel — so pass `--file` with the real binary the script eventually launches.

```sh
# redis is a shell-launcher image, so name the real binary with --file.
jerboa pkg from-docker redis:7.2 redis:7.2 --file /usr/local/bin/redis-server
# unikernel.toml: lang="raw", pkgs=["redis:7.2"], pkg_source="jerboa",
#                 [program] path="/usr/local/bin/redis-server"
jerboa build . --name redis
```

`pkg create` flags:

- `--libs` — additional files to bundle (repeatable)
- `--description`, `--runtime` — metadata
- `--missing-files` — report shared libraries missing from the local filesystem
- `--sysroot <dir>` — resolve shared libraries against `<dir>` (the rootfs the binary
  was built for) instead of the host, avoiding version mismatch for foreign binaries.
  When omitted, `pkg create` still warns if the host libraries do not satisfy the
  binary's symbol versions.

---

## Network And DNS Commands

### `jerboa network create <name>`

Flags:

- `--subnet`
- `--driver` (currently `bridge`)

### `jerboa network ls`
### `jerboa network inspect <name>`
### `jerboa network rm <name>`

### `jerboa dns resolve <name>`
### `jerboa dns resolve-all <name>`
### `jerboa dns list`

Each DNS command accepts `--network` where relevant.

---

## Volume Commands

### `jerboa volume create <name>`

Flags:

| Flag | Description |
|---|---|
| `--size` | Volume size, default `1G` |
| `--seed-pkg` | Package to seed the volume from right after creation (repeatable) |
| `--pkg-source` | Source for `--seed-pkg`: `ops` (default) or `jerboa` |
| `--src` | In-package subtree whose contents become the volume root (default `/`) |

Create and seed in one step:

```sh
jerboa volume create pgdata --size 1G \
  --seed-pkg eyberg/postgresql:11.3.0 --src /db
```

### `jerboa volume seed <name>`

Populate an existing volume with an initialized filesystem taken from one or
more packages, so the data is present the first time the volume is mounted.
Created volumes are empty; mounting an empty volume over a data directory
shadows whatever the image baked there. Seeding writes the package files into
the volume's filesystem (via `mkfs` on the daemon) so the data persists across
VM lifecycles.

The canonical use is making a database persistent: a package such as
`eyberg/postgresql` ships a pre-initialized data directory (`initdb` cannot run
inside a unikernel — it fork/execs helper processes, which Nanos does not
support), which is seeded onto a volume and then mounted.

```sh
jerboa volume create pgdata --size 1G
jerboa volume seed pgdata --pkg eyberg/postgresql:11.3.0 --src /db
jerboa run postgresql -v pgdata:/db --network pgnet -p 5432:5432
```

Flags:

| Flag | Description |
|---|---|
| `--pkg` | Package providing the seed files (repeatable) |
| `--pkg-source` | `ops` (default) or `jerboa` |
| `--src` | In-package subtree whose contents become the volume root (default `/`); e.g. `/db` |

Notes:

- `--src /db` rebases the package's `/db` contents to the volume root, so
  mounting the volume at `/db` (`-v <name>:/db`) restores them in place.
- Re-running `volume seed` reformats and re-populates the volume, discarding any
  data written since the last seed — use it to reset a volume to a clean state.

### `jerboa volume ls`
### `jerboa volume inspect <name>`
### `jerboa volume rm <name>`

Volume removal and seeding are rejected while any VM references the volume,
including stopped VMs. Remove those VMs first.

---

## Compose Commands

### `jerboa compose up <file>`
### `jerboa compose down <file>`
### `jerboa compose ps <file>`
### `jerboa compose logs <file> <service>`

Current behavior to know:

- top-level volumes are auto-created on `compose up`
- declared networks are auto-created on `compose up`
- `compose down` stops **and removes** the stack's service VMs (no stopped
  remnants left behind), and removes the networks it created
- `compose down --volumes` additionally removes volumes created by that stack
- each compose file has its own state, identified by its canonical path
- partial deployments retain state for cleanup; repeated `compose down` is safe
- `compose logs` is snapshot-only; there is no follow mode today
- a service `health_check` uses the same grammar as `run --health-check`:
  `tcp:PORT` or `http:PORT:/path` (e.g. `"tcp:8080"`, `"http:8080:/healthz"`)

---

## Cluster Commands

### `jerboa node ls`

Requires the daemon to run with `--cluster-addr`.

---

## Kernel Commands

### `jerboa kernel check`
### `jerboa kernel update`

`kernel update` accepts `-y, --yes` to skip the confirmation prompt.

These manage the cached toolchain (`mkfs`, `boot.img`, `kernel.img`) independently of the CLI version. Both resolve the latest kernel from the signed release manifest at `releases.jerboa.dev` and verify each artifact against its recorded SHA-256; there is no GitHub fallback and no historical version selection.

---

## Version Command

### `jerboa version`

Read-only readout of the installed CLI and kernel versions alongside the latest published version of every component (CLI, kernel, daemon, distro, desktop) taken from the signed release manifest. It never installs anything: on Windows the toolset is updated through the Jerboa Desktop app; on Linux you reinstall the CLI/daemon yourself. (`jerboa --version` still prints just the CLI version.)

Add `--channel beta` to query the beta channel instead of stable.

---

## Config Commands

### `jerboa config get <key>`
### `jerboa config set <key> <value>`

The command group currently exposes only one writable key:

- `hypervisor`: `qemu` or `firecracker`

The config file supports more fields than the CLI subcommand currently edits. The underlying schema in `~/.jerboa/config.toml` also includes:

- `[daemon] endpoint`
- `[daemon] distro`
- `[daemon] jerboad_path`
- `[daemon] token`

Those are read by the codepath even though `jerboa config set` does not edit them yet.

With `hypervisor = "qemu"`, the daemon automatically enables KVM when
`/dev/kvm` is accessible by adding `-enable-kvm -cpu host`. If KVM is not
available, QEMU falls back to TCG emulation and the daemon logs a warning.
Firecracker still requires KVM.

---

## Windows Daemon Commands

These commands exist only on Windows.

### `jerboa daemon install`

Import the dedicated WSL2 distro. Before replacing an existing installation,
the CLI downloads and validates the replacement and exports a recovery backup.
The backup path is printed and retained if replacement fails. Local `--rootfs`
archives are also validated before any unregister operation.

Flags:

- `--rootfs <tarball>`
- `--force`

### `jerboa daemon reinstall`

Reimport the distro rootfs (fresh or newer). Unlike `install --force`, which
destroys everything, `--keep-data` preserves your images, VMs, networks and volumes
across the swap (exported before, restored after). The daemon is stopped and
restarted around the reimport; the kernel toolchain cache is not preserved (it
re-downloads on demand).

Flags:

- `--rootfs <tarball>` — use a local rootfs instead of downloading the release
- `--keep-data` — preserve images, VMs, networks and volumes across the reimport
- `--hypervisor <qemu|firecracker>` — hypervisor to run after restart

### `jerboa daemon uninstall`
### `jerboa daemon start`
### `jerboa daemon restart`
### `jerboa daemon stop`
### `jerboa daemon status`
### `jerboa daemon logs`

Daemon start/restart flag:

- `--hypervisor qemu|firecracker`

`jerboa daemon logs` flag:

- `-f, --follow`

The daemon runs as `root` inside the dedicated distro. The client persists rendezvous state in `~/.jerboa/daemon.json`.

---

## Native `jerboad` Flags

`jerboad` is the Linux daemon binary.

| Flag | Description |
|---|---|
| `-H, --host` | Listen endpoint |
| `--auth-token` | Shared secret for `Auth.Hello` (env: `JERBOA_AUTH_TOKEN`); empty disables auth |
| `--insecure` | Allow serving a TCP endpoint without an auth token (unsafe). A TCP endpoint with no token is rejected at startup otherwise; a Unix socket needs no token (it is restricted to the owning user with `0600` permissions) |
| `--qemu` | QEMU binary path (default `qemu-system-x86_64`) |
| `--hypervisor` | `qemu` or `firecracker` (overrides `~/.jerboa/config.toml`) |
| `--fc-bin` | Firecracker binary path (only with `--hypervisor=firecracker`) |
| `--fc-kernel` | Firecracker-compatible kernel path (auto-downloaded if omitted) |
| `--tools-dir` | Toolchain cache/lookup directory (`mkfs`, `boot.img`, `kernel.img`); empty caches under `~/.jerboa/tools` |
| `--store` | Image store root directory (default `~/.jerboa/images`) |
| `--vm-store` | VM state store backend: `file` (default) or `sqlite` |
| `--vm-log-max-bytes` | Max in-memory serial log bytes retained per VM (`0` = 4 MiB default) |
| `--log-format` | Log format: `text` (default) or `json` |
| `--metrics-addr` | HTTP address for Prometheus metrics (e.g. `:9090`); empty disables metrics |
| `--ui-addr` | HTTP address for the web dashboard (e.g. `:8080`); empty disables it |
| `--trace-addr` | OTLP gRPC address for trace export (e.g. `localhost:4317`); empty disables tracing |
| `--cluster-addr` | HTTP address for the cluster gossip endpoint (e.g. `:7946`); empty disables cluster |
| `--join` | Comma-separated seed node addresses to join (e.g. `10.0.0.2:7946,10.0.0.3:7946`) |

Observability and store flags (`--metrics-addr`, `--ui-addr`, `--trace-addr`,
`--log-format`, `--vm-log-max-bytes`, `--vm-store`) are covered in more detail in
[Observability]({% link observability.md %}).

---

## Current Command Surface

Root commands currently exposed by the built CLI:

- `build`
- `compose`
- `config`
- `daemon` (Windows only)
- `dns`
- `exec`
- `images`
- `init`
- `inspect`
- `kernel`
- `logs`
- `network`
- `node`
- `pkg`
- `ps`
- `rm`
- `rmi`
- `run`
- `sign`
- `stats`
- `status`
- `stop`
- `verify`
- `version`
- `volume`
