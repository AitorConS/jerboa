# Jerboa

Jerboa is a unikernel engine for building, running, and orchestrating VM-based application images.

It is built around two binaries:

- `jerboa`: CLI
- `jerboad`: daemon (Linux; native macOS ARM64 development preview)

The current project state is best described as **public beta**:

- the core architecture is real and coherent
- the feature surface is already meaningful
- the operational matrix is still opinionated and not friction-free

## Current Status

What exists today:

- image builds from static ELF binaries and source projects
- build drivers for `go`, `node`, `python`, `rust`, and `raw`
- VM execution on QEMU and Firecracker
- managed bridge networks with TAP-backed guests
- internal DNS
- volumes
- compose stacks
- daemon-side metrics, traces, stats, and dashboard
- Windows support through a dedicated WSL2 distro hosting the daemon

What this is not yet:

- a general-purpose Docker replacement
- a zero-friction cross-platform runtime
- a fully stable networking/runtime surface for all workloads

## Platform Model

Jerboa provides these host modes:

- **Linux host**: `jerboa` talks to a native `jerboad`
- **macOS Apple Silicon (preview)**: native ARM64 CLI/daemon with QEMU/HVF, userspace networks and optional explicit x86 emulation
- **Windows host**: `jerboa.exe` talks to `jerboad` running inside a dedicated WSL2 distro managed by `jerboa daemon`

Important:

- Native macOS ARM64 development preview: see [macOS Apple Silicon](docs/macos.md)
- Stable releases currently target Linux and Windows/WSL2
- Linux uses KVM when available; macOS ARM64 uses HVF
- Windows support is WSL2-based, not a native Windows daemon port

## Quick Start

### Linux

```bash
sudo scripts/install.sh
jerboa status
jerboa build examples/hello --name hello --lang go
jerboa run hello:latest --attach
```

### Windows

```powershell
jerboa daemon install
jerboa daemon start
jerboa status
jerboa build examples/hello --name hello --lang go
jerboa run hello:latest --attach
```

New to unikernels? `jerboa init` scaffolds a commented `unikernel.toml`,
builds run preflight checks that catch boot failures before they happen, and
`jerboa build --smoke` boot-tests the image as part of the build. Start with
[Build Concepts](docs/build-concepts.md).

## Documentation

- [Getting Started](docs/getting-started.md)
- [Build Concepts](docs/build-concepts.md) — what a unikernel build actually does, explained from zero
- [CLI Reference](docs/cli-reference.md)
- [Compose](docs/compose.md)
- [Architecture](docs/architecture.md)
- [Observability](docs/observability.md)
- [Troubleshooting](docs/troubleshooting.md) — common errors, decoded

The short version:

- port publishing requires `--network`
- TCP forwarding works today
- UDP mappings are accepted syntactically but are not forwarded yet
- the Windows path depends on WSL2
- the public surface is broader than the amount of battle-hardening so far

## Repository Layout

- `cmd/jerboa/` - CLI
- `cmd/jerboad/` - daemon
- `internal/vm/` - VM lifecycle and hypervisor backends
- `internal/image/` - image store and build path
- `internal/network/` - bridge, TAP, IPAM, and port forwarding
- `internal/compose/` - compose parser and ordering
- `internal/wslboot/`, `internal/wsldistro/` - Windows/WSL support
- `tests/integration/`, `tests/e2e/` - higher-level tests
- `kernel/` - Nanos-derived kernel tree and tooling

## Development

Common commands:

```bash
make build
make test
make test-integration
make e2e
make lint
```

For contribution expectations, see [CONTRIBUTING.md](CONTRIBUTING.md).


## Security

For reporting guidance, see [SECURITY.md](SECURITY.md).

## License

This repository is licensed under **Apache-2.0**.

See:

- [LICENSE](LICENSE)
- [NOTICE](NOTICE)

The `kernel/` subtree keeps its own notices and license file where applicable.
