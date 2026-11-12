# Contributing

Thanks for contributing to Jerboa.

This project spans:

- Go CLI and daemon code
- Linux/WSL integration
- VM lifecycle and networking
- a Nanos-derived kernel/toolchain tree

Keep changes narrow and technically defensible.

## Before You Start

Read first:

- [README.md](README.md)
- [KNOWN_LIMITATIONS.md](KNOWN_LIMITATIONS.md)
- docs under `docs/`

Make sure your change matches the current project posture. Small, concrete improvements are much easier to review than speculative expansion.

## Development Setup

Typical commands:

```bash
make build
make test
make lint
```

Additional suites:

```bash
make test-integration
make e2e
make kernel
make test-kernel
```

Some test paths require:

- Linux
- `/dev/kvm`
- runner access to virtualization features

## Change Scope

Preferred contributions:

- correctness fixes
- test coverage for existing behavior
- documentation corrections
- operational hardening
- small UX improvements with low ambiguity

Use more caution with:

- networking semantics
- Windows/WSL lifecycle changes
- Firecracker behavior
- kernel/toolchain changes
- broad CLI behavior changes

## Coding Expectations

- follow existing code patterns
- keep edits localized
- add tests proportional to risk
- avoid unrelated refactors in the same change
- update docs when behavior changes

## Performance Work

- Read [the macOS performance log](docs/benchmarks/macos-performance-log.md)
  before an experiment, and add its verdict to the log when it ends.
- Push your branch and open a draft pull request when you start, not when you
  finish. Kernel, VMM and daemon changes are measured against each other, and
  a branch that only exists on one machine cannot be seen by whoever is
  measuring the same area.
- Show the comparison (`scripts/benchmark-compare.py`) in the pull request.

## Pull Requests

A good PR should state:

1. what changed
2. why it changed
3. what you verified
4. what remains risky or unverified

If a change is platform-specific, say so explicitly.

## Documentation Changes

If you change:

- CLI flags
- daemon behavior
- networking behavior
- build behavior
- Windows/WSL workflows

then update the relevant docs in `docs/` and any root-level public docs affected by the change.

## Security

For sensitive issues, do not open a public bug with exploit details first. Follow [SECURITY.md](SECURITY.md).
