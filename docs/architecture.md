---
layout: default
title: Architecture
nav_order: 5
---

# Architecture
{: .no_toc }

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

## High-Level Shape

Jerboa is a client/daemon system:

- `jerboa` parses command-line input
- `jerboad` owns the real state and runtime actions
- JSON-RPC connects the two

On Linux, the default transport is a Unix socket.
On Windows, the client boots and dials a daemon running inside a dedicated WSL2 distro.

## Main Components

### CLI

Path: `cmd/jerboa/`

Responsibilities:

- Cobra command surface
- config resolution
- local packaging/build context preparation
- JSON / table formatting
- Windows daemon bootstrap path

### Daemon

Path: `cmd/jerboad/`

Responsibilities:

- expose the RPC API
- manage image builds
- manage image storage
- manage VM lifecycle
- manage networks, compose workflows, and cluster membership
- expose observability endpoints

### VM Layer

Path: `internal/vm/`

Backends:

- QEMU
- Firecracker

Capabilities:

- create/start/stop/remove
- persistence through file or SQLite stores
- runtime stats
- health checking
- restart policies
- serial log buffering

### Image Layer

Path: `internal/image/`

The daemon stores images by digest under `~/.jerboa/images/` and keeps name/tag references separately.

The build path:

1. client prepares build context
2. client streams it to daemon
3. daemon resolves `mkfs`
4. daemon writes the image into its own store

### Networking

Paths:

- `internal/network/`
- `internal/vm/portmap.go`

Current model:

- managed bridge networks
- one TAP per VM
- host-side IPAM
- internal DNS records
- TCP port publishing through a userspace forwarder

Important constraints:

- no SLIRP fallback
- `-p` requires `--network`
- UDP mappings are parsed but not forwarded yet

### Compose

Path: `internal/compose/`

The compose package only parses and orders stack definitions. The CLI command layer drives the actual orchestration through the daemon and client-side volume store.

### Windows Support

Paths:

- `internal/wslboot/`
- `internal/wsldistro/`
- `cmd/jerboa/daemon.go`

Windows support is not a native daemon port. Instead:

- the CLI provisions a dedicated WSL2 distro
- the daemon runs there as Linux
- the client persists the rendezvous token and endpoint in `~/.jerboa/daemon.json`

## Config Resolution

Path: `internal/config/`

Supported config schema:

```toml
hypervisor = "qemu"

[daemon]
endpoint = "unix:///var/run/jerboad.sock"
distro = "jerboa"
jerboad_path = "/usr/local/bin/jerboad"
token = "..."
```

The `jerboa config` subcommand currently edits only `hypervisor`, but the code reads the full schema.

## Guest Injection Paths

Environment variables and static network configuration are injected through fw_cfg using:

- `opt/uni/env`
- `opt/uni/network`

Recent code and docs use the `opt/uni/*` keys; older `opt/jerboa/*` names are stale.

## Persistence

Client-owned local state:

- config file
- WSL daemon rendezvous file
- package caches
- volume store
- compose state file

Daemon-owned state:

- image store
- VM registry
- network store

VM persistence backends:

- file
- SQLite

Volumes are raw TFS disk images created (sparse) on the client and labeled by the
daemon. They are formatted lazily on first attach, or pre-populated up front with
`jerboa volume seed`: the client streams a package subtree to the daemon, which
builds a children-only Nanos manifest and runs `mkfs` to write the files into the
volume's filesystem. This lets a volume carry initialized data (e.g. a database
cluster that cannot be initialized inside the single-process unikernel) before it
is ever mounted.

## Tests And CI

The repo currently contains:

- unit tests across `cmd/` and `internal/`
- integration tests in `tests/integration/`
- e2e tests in `tests/e2e/`
- kernel tests under `kernel/test/`

CI also builds release binaries, a WSL distro rootfs artifact, and benchmark jobs for QEMU and Firecracker boot paths.
