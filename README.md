# Jerboa

Jerboa is a unikernel engine for building, running, and orchestrating VM-based application images.

It is built around two binaries:

- `jerboa`: CLI
- `jerboad`: Linux daemon

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
- replicated services
- daemon-side metrics, traces, stats, and dashboard
- Windows support through a dedicated WSL2 distro hosting the daemon

What this is not yet:

- a general-purpose Docker replacement
- a zero-friction cross-platform runtime
- a fully stable networking/runtime surface for all workloads

## Platform Model

Jerboa runs in two supported modes:

- **Linux host**: `jerboa` talks to a native `jerboad`
- **Windows host**: `jerboa.exe` talks to `jerboad` running inside a dedicated WSL2 distro managed by `jerboa daemon`

Important:

- `jerboad` is Linux-only
- native VM execution depends on Linux virtualization support
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

## Documentation

- [Getting Started](docs/getting-started.md)
- [CLI Reference](docs/cli-reference.md)
- [Compose](docs/compose.md)
- [Architecture](docs/architecture.md)
- [Observability](docs/observability.md)

## Known Limits

Before using Jerboa seriously, read:

- [Known Limitations](KNOWN_LIMITATIONS.md)

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
- `internal/service/` - replicated services
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

## Public Beta Positioning

If you are opening the repository publicly, the safest framing is:

- public beta
- developer preview
- experimental but usable for interested systems engineers

Avoid framing it as stable infrastructure for arbitrary production workloads without qualification.

## Security

For reporting guidance, see [SECURITY.md](SECURITY.md).

## License

This repository is licensed under **Apache-2.0**.

See:

- [LICENSE](LICENSE)
- [NOTICE](NOTICE)

The `kernel/` subtree keeps its own notices and license file where applicable.
