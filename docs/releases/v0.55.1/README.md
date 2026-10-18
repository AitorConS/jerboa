# Jerboa v0.55.1 validation evidence

Candidate: [39086609415](https://github.com/AitorConS/jerboa/actions/runs/39086609415).

- Engine: `c345513040ab359ff6664a780ea090e09c9f6b23`
- Desktop: `470e957098f610518078c917ed3b35b5306013e0`
- Firecracker macOS: `c7b50ad010f14b9b5598636c4826c4a0ab3c5d62`
- Product: `v0.55.1`; Linux kernel toolset: stable `v0.2.2`, reused.

## What changes

The release ships the redesigned Jerboa Desktop ([JerboaDesktop#12](https://github.com/AitorConS/JerboaDesktop/pull/12)). The engine is unchanged from v0.55.0 except for documentation descriptions and the version ([#133](https://github.com/AitorConS/jerboa/pull/133)). The firecracker-macos runtime is the same commit as v0.55.0. No Linux kernel changes, so the candidate reuses the published toolset (`kernel_source=stable`).

## Native Mac

The actual candidate app ZIP passed `scripts/release/native-macos.sh` on a physical Apple Silicon Mac running macOS 26.6.2. The checkout matched the candidate engine commit. The test used an isolated daemon and temporary VM state.

App: `jerboa-desktop-0.55.1-macos-arm64.zip`.

SHA-256: `97d6ab591e67c331330482399d341925f297c1166ac98e9e5d51a68e0d8ac17b` (matches `native_macos_app.sha256` in the candidate's signed inventory).

[Machine-readable result](native-macos.json) and [test output](native-macos.log).

Checks passed: app code-signature integrity, CLI/daemon versions, VM boot with 1 and 4 vCPUs, HTTP/UDP port forwarding, environment variables, persistent-volume contents across boots, port conflicts, daemon recovery, guest exit, restart after guest failure, and forced stop. These are functional checks, not throughput benchmarks.

## Linux kernel

The reused stable toolset passed the QEMU/KVM 8.2.2 and Firecracker 1.16.0 boot checks in this candidate. The [kernel report](kernel-boot.json) is the candidate inventory's `kernel_boot` record, binding both results to the engine commit, candidate run and SHA-256 of each toolset file.

## Windows

Windows packaging ran on hosted runners and is in the candidate. The installer smoke does not run in the candidate workflow ([#130](https://github.com/AitorConS/jerboa/pull/130)); the WSL path is unchanged from v0.55.0.

## Publication gate

This directory records validation, not proof of publication. Promotion additionally requires the complete candidate workflow to succeed and the signed inventory to match this native app hash. Published versions are determined by the signed stable manifest at `https://releases.jerboa.dev/channels/stable.json`.
