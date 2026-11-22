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

The daemon runs natively on Linux and, as a development preview, macOS Apple
Silicon. On Windows, the `jerboa` CLI runs on the host and talks to `jerboad`
running inside a dedicated WSL2 distro managed by `jerboa daemon`.
See [macOS Apple Silicon](docs/macos.md) for the native build and compatibility matrix.
See [Linux ARM64 packages](docs/packages-arm64.md) for local sysroot packaging,
Docker imports, package provenance and Spark acceptance.

## Current Capabilities

- **Build** unikernel images from:
  - static ELF binaries
  - Go, Node.js, Python, Rust, and `raw` (package-driven) projects
- **Run** VMs on:
  - a microVM engine, the default wherever the machine can run it: Firecracker
    on Linux and WSL2 with KVM, and on Apple Silicon with macOS 26
    [Spark](https://github.com/AitorConS/Spark), our Hypervisor.framework fork
    of Firecracker
  - QEMU otherwise (KVM on Linux when available; HVF for native ARM64 guests on macOS)
- **Distribute** images through a registry (`registry.jerboa.dev` by default):
  - `push` uploads an image's recipe and only the files the registry lacks; `pull` rebuilds the disk locally and verifies it byte for byte
  - Sigstore signatures from trusted publishers, verified before anything is downloaded
  - official images under `jerboa/` (Redis, nginx, Traefik, Prometheus and more), browsable on [hub.jerboa.dev](https://hub.jerboa.dev)
  - `jerboa run` pulls an image it does not have yet
- **Manage**:
  - images and volumes
  - in-place VM snapshots (macOS, Spark)
  - bridge networks with TAP-backed guest connectivity
  - internal DNS for service discovery
  - compose stacks
- **Observe**:
  - live VM serial logs and stats
  - Prometheus metrics and OTLP traces from the daemon
  - a read-only dashboard
- **Maintain**:
  - `jerboa doctor` checks the machine, the daemon and its stored data
  - `jerboa system df` and `jerboa system prune` show and reclaim disk space

### Runtime constraints worth knowing up front

- Stable releases target Linux and Windows/WSL2; native macOS ARM64 is a development preview.
- On Linux, guests reach neither the host nor private networks, and a VM on a network needs `nftables` for that isolation.
- On Linux, port publishing requires a managed network (`-p/--port` requires `--network`) — there is no SLIRP fallback.
- Linux publishes TCP and UDP ports through kernel NAT (nftables), the default where it is available. The userspace proxy, used with `jerboad --port-publish proxy`, on WSL2 and where kernel NAT is missing, forwards TCP only and skips UDP mappings with a warning.
- macOS provides userspace networks, guest DNS, TCP/UDP forwarding and optional explicit x86 emulation.
- macOS uses CPU priority and an RSS watchdog as resource controls; these do not provide Linux cgroup isolation guarantees.

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

# Or run an official image: the first run pulls it and checks its signature
jerboa run jerboa/redis
```

For networking, volumes, DNS, and compose stacks, see
[Getting Started](docs/getting-started.md).

## Installing

### macOS Apple Silicon

Spark requires macOS 26+. Releases publish a self-contained Jerboa
Desktop DMG (the `desktop` component's `darwin-arm64` asset). Its app bundles
the CLI, daemon, Spark, its libraries and kernel tools,
starts the user daemon through launchd, offers to add `jerboa` to
`/usr/local/bin` and updates itself from inside the app. It is not
Developer ID signed or notarized yet, so allow its first launch in System
Settings → Privacy & Security. The macOS path of `install.sh` needs a signed
installer package, which releases do not publish yet. See
[Spark installation and source builds](docs/macos-spark.md) and
[release infrastructure requirements](docs/macos-distribution.md).

### Linux

One-shot installer (installs `jerboa`, `jerboad`, QEMU/Firecracker, and a
systemd service):

```bash
curl -fsSL https://jerboa.dev/install.sh | sudo bash
jerboa status
```

`jerboa doctor` checks the result. To remove everything, including images
and volumes, run the installer with `--uninstall` (`--keep-data` keeps them).

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

Requires **Go 1.27+** (`go.mod` pins the toolchain).

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
cmd/jerboa-verify/  release manifest verification
cmd/jerboa-manifest/manifest tooling
cmd/jerboa-smoke/, cmd/jerboa-bench/, cmd/jerboa-determinism/   test and measurement tools
internal/vm/        VM lifecycle: QEMU, Firecracker, stats, persistence
internal/image/     image store and build pipeline
internal/network/   bridge, TAP, IPAM, port forwarder
internal/compose/   compose parser and ordering
internal/registry/, internal/ociregistry/, internal/ociblob/   image registry client
internal/snapshot/, internal/volume/   snapshots and volumes
internal/durable/   crash-safe state files
internal/release/, internal/signing/   signed releases and updates
internal/wsldistro/ and internal/wslboot/   Windows/WSL2 integration
kernel/             Nanos-derived kernel tree and tooling
distro/             Windows WSL2 distro rootfs build
scripts/            installers, package build scripts and release tooling (scripts/release/)
examples/           sample projects (Go, Node, Python, Ruby, databases, ...)
docs/               documentation source consumed by Jerboa_Public_Docs
tests/integration/, tests/e2e/, tests/upgrade/   higher-level test suites
```

## Documentation

Full documentation is published at [docs.jerboa.dev](https://docs.jerboa.dev),
maintained and deployed from [Jerboa_Public_Docs](https://github.com/AitorConS/Jerboa_Public_Docs).
Its sync workflow still consumes [`docs/`](docs/) from this repository; the engine
no longer builds or deploys the old Jekyll site. Local source references:

- [Getting Started](docs/getting-started.md)
- [Build Concepts](docs/build-concepts.md)
- [CLI Reference](docs/cli-reference.md)
- [Compose](docs/compose.md)
- [Architecture](docs/architecture.md)
- [Observability](docs/observability.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Changelog](CHANGELOG.md)

## CI and releases

CI validates changes without publishing. The candidate workflow builds every platform
from pinned revisions; a separate reviewed promotion publishes the verified artifacts
and synchronizes website downloads. See [release operations](docs/release-process.md).

## Versioning

- CLI/daemon version: [`VERSION.md`](VERSION.md) (source version; wire protocol 3)
- Kernel version: [`kernel/VERSION`](kernel/VERSION)
- What changed in each version: [`CHANGELOG.md`](CHANGELOG.md)

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
