---
layout: default
title: CLI Reference
nav_order: 3
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
| `--output table|json` | Output format |
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
| `-p, --port` | Port mapping `host:guest[/tcp|udp]` |
| `-e, --env` | Repeatable environment variable |
| `--env-file` | Read env vars from file |
| `--name` | Human-readable VM name |
| `--rm` | Auto-remove after stop |
| `-v, --volume` | Named volume mount `name:path[:ro]` |
| `--attach` | Stream serial console and block |
| `-d, --detach` | Detached mode, default `true` |
| `--network` | Managed network name |
| `--ip` | Static guest IP, requires `--network` |
| `--health-check` | `tcp:PORT` or `http:PORT:/path` |
| `--restart` | `never`, `on-failure`, `always[:max-retries]` |
| `--verify` | Signature verification mode: `off`, `warn`, `enforce` |
| `--cpu-shares` | cgroup v2 CPU weight |
| `--memory-max` | cgroup v2 memory hard limit |
| `--disk-iops` | Boot-disk IOPS throttle |
| `--disk-bps` | Boot-disk throughput throttle |

Notes:

- Port publishing requires `--network`.
- TCP forwarding works today.
- UDP mappings are currently skipped by the userspace forwarder with a warning.

### `jerboa ps`

List VMs known to the daemon. The command help still says "running", but the implementation prints the daemon registry, including stopped entries when present.

### `jerboa status`

Summary view of daemon-side VM counts and restart/health state.

### `jerboa logs <id>`

Print buffered serial console output.

Flag:

- `-f, --follow`: poll and stream appended output until the VM stops

### `jerboa inspect <id>`

Return full VM detail as JSON.

### `jerboa stats <id>`

Show runtime stats.

Flags:

- `-w, --watch`
- `-i, --interval`

### `jerboa stop <id>`

Graceful stop by default.

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
| `--pkg` | Include runtime package(s) |
| `--pkg-source` | `jerboa` or `ops` |
| `--lang` | `go`, `node`, `python`, `rust`, `raw` |
| `--platform` | Cross-build target |
| `--port` | Declared service port; emits the guest network section in the build manifest |

`unikernel.toml` is read automatically when present.

### `jerboa images`

List images stored by the daemon.

### `jerboa rmi <ref>`

Remove an image from the daemon store.

### `jerboa sign <image>`

Resolve the image through the daemon and sign its disk digest with the default Ed25519 key.

### `jerboa verify <image>`

Resolve the image through the daemon and verify its stored signature.

---

## Package Commands

`jerboa pkg` manages runtime packages used during builds.

Subcommands:

| Command | Purpose |
|---|---|
| `pkg list` | List locally cached packages |
| `pkg search <query>` | Search remote index |
| `pkg get <ref>` | Download package |
| `pkg remove <ref>` | Remove cached package |
| `pkg create <name>[:version] <binary>` | Create a local package |
| `pkg from-docker <name>[:version] <image>` | Extract a binary and libraries from a Docker image |
| `pkg push <name>:<version> <index-url>` | Push a local package to a remote index |
| `pkg load <package>` | Download, build, and prepare a runnable image in one step |

Supported package sources:

- `jerboa`
- `ops`

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

Flag:

- `--size`, default `1G`

### `jerboa volume ls`
### `jerboa volume inspect <name>`
### `jerboa volume rm <name>`

---

## Service Commands

### `jerboa service run <name> <image>`

Flags:

- `--replicas`
- `--memory`
- `--cpus`
- `--env`
- `--network`
- `--strategy`
- `--health-timeout`

### `jerboa service scale <name> <replicas>`
### `jerboa service update <name> <image>`
### `jerboa service ls`
### `jerboa service inspect <name>`
### `jerboa service rm <name>`

---

## Compose Commands

### `jerboa compose up <file>`
### `jerboa compose down <file>`
### `jerboa compose ps <file>`
### `jerboa compose logs <file> <service>`

Current behavior to know:

- top-level volumes are auto-created on `compose up`
- declared networks are auto-created on `compose up`
- `compose down --volumes` removes only volumes created by that stack
- services with `replicas > 1` are deployed through the service manager
- `compose logs` is snapshot-only; there is no follow mode today

---

## Cluster Commands

### `jerboa node ls`

Requires the daemon to run with `--cluster-addr`.

---

## Kernel Commands

### `jerboa kernel check`
### `jerboa kernel update`
### `jerboa kernel list`
### `jerboa kernel use <version>`

These manage the cached toolchain (`mkfs`, `boot.img`, `kernel.img`) independently of the CLI version.

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

---

## Windows Daemon Commands

These commands exist only on Windows.

### `jerboa daemon install`

Import the dedicated WSL2 distro.

Flags:

- `--rootfs <tarball>`
- `--force`

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
| `--auth-token` | Shared secret for `Auth.Hello` |
| `--qemu` | QEMU binary path |
| `--hypervisor` | `qemu` or `firecracker` |
| `--fc-bin` | Firecracker binary path |
| `--fc-kernel` | Firecracker-compatible kernel path |
| `--tools-dir` | Toolchain cache/lookup directory |
| `--store` | Image store root |
| `--vm-store` | `file` or `sqlite` |
| `--vm-log-max-bytes` | Per-VM in-memory serial log retention; `0` uses the built-in 4 MiB default |
| `--metrics-addr` | Metrics HTTP bind |
| `--ui-addr` | Dashboard HTTP bind |
| `--log-format` | `text` or `json` |
| `--trace-addr` | OTLP gRPC target |
| `--cluster-addr` | Cluster gossip bind |
| `--join` | Comma-separated seed list |

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
- `service`
- `sign`
- `stats`
- `status`
- `stop`
- `verify`
- `volume`
