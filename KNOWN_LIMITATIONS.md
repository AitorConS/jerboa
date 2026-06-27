# Known Limitations

This file describes the current limits of Jerboa as it exists today, not the intended long-term shape.

## Release Posture

Jerboa is suitable for:

- public beta
- architecture review
- contributor onboarding
- targeted experimentation by systems-minded users

Jerboa is not yet suitable to describe as universally stable infrastructure.

## Platform Constraints

- `jerboad` is Linux-only.
- Windows support works by running the daemon inside a dedicated WSL2 distro.
- Native VM execution depends on Linux virtualization support and host configuration.

## Networking

- Port publishing requires a managed network (`--network`).
- There is no SLIRP fallback path.
- TCP publishing works through a userspace forwarder.
- UDP mappings parse and persist, but the current forwarder skips them with a warning.
- Network behavior is more constrained than container users typically expect.

## Windows/WSL2

- The Windows experience depends on WSL2 being available and healthy.
- The daemon lifecycle, distro import, and host-to-guest routing path are more operationally complex than a native Windows service.
- Support quality on Windows is only as strong as the dedicated WSL2 path.

## Build Surface

- The build feature set is broad, but each language/runtime path does not yet imply equal runtime maturity.
- `raw` mode is powerful but assumes the user understands the runtime/package shape they are building.
- Framework-oriented builds still require project-specific judgment (`unikernel.toml`, build steps, runtime packages).

## Operational Complexity

- QEMU and Firecracker expand the test matrix.
- Linux native mode and Windows WSL2 mode expand the operational matrix.
- Kernel/toolchain management is separate from CLI versioning.

## Product Positioning Risk

Users will naturally compare Jerboa to:

- Docker
- Podman
- Firecracker tooling
- lightweight VM orchestrators

If the public messaging is too broad, users will assume portability and behavior the project does not claim yet.

## Documentation Risk

The documentation now tracks the code much more closely, but the project is still evolving quickly enough that:

- command help
- docs
- runtime behavior

need periodic resync.

## Repository Readiness Gaps

The main public-facing repo risks are not architectural so much as packaging and expectation-setting:

- no root-level public README before this pass
- no explicit public-beta framing unless you add it

## Practical Recommendation

Open the repository publicly as:

- **public beta**, or
- **experimental developer preview**

Do not market it as a drop-in production runtime without a much narrower support statement.
