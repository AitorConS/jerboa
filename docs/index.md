---
layout: home
title: Home
nav_order: 1
---

# Jerboa
{: .fs-9 }

Unikernel engine for building, running, and orchestrating VM-based application images.
{: .fs-6 .fw-300 }

[Get Started]({% link getting-started.md %}){: .btn .btn-primary .fs-5 .mb-4 .mb-md-0 .mr-2 }
[View on GitHub](https://github.com/AitorConS/jerboa){: .btn .fs-5 .mb-4 .mb-md-0 }

---

## What It Is

Jerboa is split into two parts:

- `jerboa`: the CLI
- `jerboad`: the daemon that owns builds, images, VM lifecycle, networks, and compose state

The daemon is Linux-only. On Windows, the CLI runs on the host and boots the daemon inside a dedicated WSL2 distro managed by `jerboa daemon`.

## Current Capabilities

- Build unikernel images from:
  - static ELF binaries
  - Go, Node.js, Python, Rust, and `raw` projects
- Run VMs on:
  - QEMU
  - Firecracker
- Manage:
  - images
  - volumes
  - bridge networks with TAP-backed guest connectivity
  - internal DNS
  - compose stacks
- Observe:
  - VM logs
  - live stats
  - Prometheus metrics
  - OTLP traces
  - a read-only dashboard

## Important Runtime Constraints

- Native VM execution requires Linux.
- Port publishing requires a managed network (`--network`).
- TCP publishing works today through a userspace forwarder.
- UDP port mappings parse and persist, but the current forwarder skips them with a warning.
- The daemon binary is built only for Linux; Windows uses WSL2.

## Repo Shape

- `cmd/jerboa/` - CLI commands
- `cmd/jerboad/` - daemon entrypoint
- `internal/vm/` - VM lifecycle, QEMU, Firecracker, stats, persistence
- `internal/image/` - image store and build pipeline
- `internal/network/` - bridge, TAP, IPAM, port forwarder
- `internal/compose/` - compose parser and ordering
- `internal/wsldistro/` and `internal/wslboot/` - Windows/WSL integration
- `tests/integration/` and `tests/e2e/` - higher-level verification
- `kernel/` - Nanos-derived kernel tree and tooling

## Versions In This Repo

- CLI version source: `VERSION`
- Kernel version source: `kernel/VERSION`

At the time of this documentation update:

- CLI version: `v0.48.0`
- Kernel version: `v0.1.2`

## Next

- [Getting Started]({% link getting-started.md %})
- [CLI Reference]({% link cli-reference.md %})
- [Compose]({% link compose.md %})
- [Architecture]({% link architecture.md %})
- [Observability]({% link observability.md %})
