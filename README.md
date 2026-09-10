# Jerboa

**Smaller than a container. Sharper than a VM.**

Jerboa is a unikernel engine: it compiles an application — a Go/Node/Python/Rust
project, a static binary, or a database from the ops package ecosystem — into a
single-purpose, bootable VM image running [**Nanos**](https://nanos.org), a
minimal, single-process kernel. No OS, no container, no shared kernel — just
your code and the metal underneath it.

Because there is no shell, no init system, no package manager, and no other
process that will ever run alongside your program, there is nothing inside the
image to exploit. What you get is the isolation of a VM with the speed and size
of a container: fast boot, a tiny attack surface, and a disk image measured in
megabytes instead of hundreds of megabytes.

- Website: [jerboa.dev](https://jerboa.dev)
- Documentation: [docs.jerboa.dev](https://docs.jerboa.dev)
- License: [Apache 2.0](LICENSE)

---

## What It Is

Jerboa is split into two parts:

- **`jerboa`** — the CLI you run locally.
- **`jerboad`** — the daemon that owns builds, images, VM lifecycle, networks,
  and compose state.

The daemon is Linux-only. On Windows, the `jerboa` CLI runs on the host and
talks to `jerboad` running inside a dedicated WSL2 distro managed by
`jerboa daemon`.

## Current Capabilities

- **Build** unikernel images from:
  - static ELF binaries
  - Go, Node.js, Python, Rust, and `raw` (package-driven) projects
- **Run** VMs on:
  - QEMU (with automatic KVM acceleration when `/dev/kvm` is available)
  - Firecracker
- **Manage**:
  - images and volumes
  - bridge networks with TAP-backed guest connectivity
  - internal DNS for service discovery
  - compose stacks
- **Observe**:
  - live VM serial logs and stats
  - Prometheus metrics and OTLP traces from the daemon
  - a read-only dashboard

### Runtime constraints worth knowing up front

- Native VM execution requires Linux; the daemon binary itself only builds for Linux.
- Port publishing requires a managed network (`-p/--port` requires `--network`) — there is no SLIRP fallback.
- TCP port forwarding works today through a userspace forwarder; UDP mappings parse and persist but are currently skipped by the forwarder with a warning.

See [Architecture](docs/architecture.md) for the full component breakdown.

## Why Nanos

Jerboa images boot [**Nanos**](https://nanos.org), a small kernel purpose-built
to run exactly one application. There is no shell to interpret scripts, no init
system, and no `fork`/`exec` — the kernel boots straight into your program and
nothing else ever runs beside it. This is what makes a unikernel fundamentally
different from a container: a container shares the host kernel and userland; a
Nanos-based image packages your app plus a minimal kernel into one bootable
disk and runs it as its own VM.

The practical implication is the **one-process model**: setup tooling that
spawns child processes (an `initdb`, a Docker-style entrypoint script) cannot
run inside the guest. Jerboa handles this by doing that work *at build time*
instead — for example, the `eyberg/postgresql` package ships a pre-initialized
data directory baked directly into the image. Read
[Build Concepts](docs/build-concepts.md) for the full explanation, including
volume seeding for stateful services.

The `kernel/` directory in this repository is Jerboa's Nanos-derived kernel and
toolchain tree — see [kernel/INTERFACE.md](kernel/INTERFACE.md).

## Quick Start

```bash
# Scaffold a project (detects the language, writes a documented unikernel.toml)
jerboa init

# Build an image
jerboa build examples/hello --name hello --lang go

# Run it
jerboa run hello:latest
jerboa ps
jerboa logs <vm-id>
```

For networking, volumes, DNS, and compose stacks, see
[Getting Started](docs/getting-started.md).

## Installing

### Linux

One-shot installer (installs `jerboa`, `jerboad`, QEMU/Firecracker, and a
systemd service):

```bash
curl -fsSL https://jerboa.dev/install.sh | sudo bash
jerboa status
```

### Windows

On Windows, `jerboa` ships with the **Jerboa Desktop app**, whose installer
puts the CLI on your `PATH`. After installing the app, import the dedicated
WSL2 distro that hosts the daemon:

```powershell
jerboa daemon install
jerboa daemon start
jerboa daemon status
```

The desktop app can also manage this runtime for you from its GUI.

## Building From Source

Requires **Go 1.25+**.

```bash
git clone https://github.com/AitorConS/jerboa.git
cd jerboa
make build
```

Artifacts are written to `dist/`. Useful targets:

| Target | Description |
|---|---|
| `make build` | Build both `jerboa` (CLI) and `jerboad` (daemon) |
| `make build-cli` | Build only the CLI |
| `make build-daemon` | Build only the daemon |
| `make -j2 build` | Build both binaries in parallel |
| `make kernel` | Build the Nanos-derived kernel tree (see `kernel/Makefile`) |
| `make test` | Run the unit test suite with race detection and coverage |
| `make test-integration` | Run integration tests (`tests/integration/`) |
| `make test-kernel` | Run kernel unit tests |
| `make e2e` | Run end-to-end tests (`tests/e2e/`) |
| `make smoke` | Build the CLI and run the smoke test binary against it |
| `make lint` | Run `golangci-lint` |
| `make coverage` | Generate an HTML coverage report |

Source builds use `-trimpath` and stripped linker flags. `distro/build.sh`
additionally builds `jerboad` for the Windows WSL2 rootfs image — see
[distro/README.md](distro/README.md).

## Repository Layout

```
cmd/jerboa/         CLI commands
cmd/jerboad/        daemon entrypoint
cmd/jerboa-agent/   local HTTP/SSE sidecar for the desktop app
cmd/jerboa-sign/    image signing tool
cmd/jerboa-manifest/manifest tooling
internal/vm/        VM lifecycle: QEMU, Firecracker, stats, persistence
internal/image/     image store and build pipeline
internal/network/   bridge, TAP, IPAM, port forwarder
internal/compose/   compose parser and ordering
internal/wsldistro/ and internal/wslboot/   Windows/WSL2 integration
kernel/             Nanos-derived kernel tree and tooling
distro/             Windows WSL2 distro rootfs build
scripts/            installers and package build scripts
examples/           sample projects (Go, Node, Python, Ruby, databases, ...)
docs/               documentation site source (docs.jerboa.dev)
tests/integration/, tests/e2e/   higher-level test suites
```

## Documentation

Full documentation is published at [docs.jerboa.dev](https://docs.jerboa.dev),
built from [`docs/`](docs/) in this repository:

- [Getting Started](docs/getting-started.md)
- [Build Concepts](docs/build-concepts.md)
- [CLI Reference](docs/cli-reference.md)
- [Compose](docs/compose.md)
- [Architecture](docs/architecture.md)
- [Observability](docs/observability.md)
- [Troubleshooting](docs/troubleshooting.md)

## Versioning

- CLI/daemon version: [`VERSION.md`](VERSION.md) (currently `0.51.2`, wire protocol 2)
- Kernel version: [`kernel/VERSION`](kernel/VERSION)

Check installed versions against the latest release:

```bash
jerboa version          # installed CLI/kernel vs latest of every component
jerboa kernel check     # is a newer kernel available?
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, test suites, and
contribution scope. In short:

```bash
make build
make test
make lint
```

## Security

See [SECURITY.md](SECURITY.md) for the security policy and how to report
issues. Jerboa is currently best treated as a public beta / experimental
systems project.

## License

Licensed under the [Apache License, Version 2.0](LICENSE). See
[NOTICE](NOTICE) for third-party attribution, including the `kernel/` subtree,
which carries its own license.
