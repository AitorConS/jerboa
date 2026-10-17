# Jerboa v0.55.0 validation evidence

Candidate: [39052504452](https://github.com/AitorConS/jerboa/actions/runs/39052504452).

- Engine: `c95a3d98e7d399683bb2ddd701330f6d2b2cae05`
- Desktop: `a9c31b938ddd06ec9ec8ff0cd259a9bb4b398f86`
- Firecracker macOS: `c7b50ad010f14b9b5598636c4826c4a0ab3c5d62`
- Product: `v0.55.0`; source-built Linux kernel toolset: `v0.2.2`.

## Native Mac

The actual candidate app ZIP passed `scripts/release/native-macos.sh` on a physical Apple Silicon Mac running macOS 26.6.2. The checkout matched the candidate engine commit. The test used an isolated daemon and temporary VM state.

App: `jerboa-desktop-0.55.0-macos-arm64.zip`.

SHA-256: `9f2753dba325dee0e925d9de6687aa0b60d8d72f3fc3bb8d221c75e33bd822f5` (matches `native_macos_app.sha256` in the candidate's signed inventory).

[Machine-readable result](native-macos.json) and [test output](native-macos.log).

Checks passed: app code-signature integrity, CLI/daemon versions, VM boot with 1 and 4 vCPUs, HTTP/UDP port forwarding, environment variables, persistent-volume contents across boots, port conflicts, daemon recovery, guest exit, restart after guest failure, and forced stop. These are functional checks, not throughput benchmarks.

## Linux kernel

The candidate's exact staged toolset passed the QEMU/KVM 8.2.2 and Firecracker 1.16.0 boot checks. The [kernel report](kernel-boot.json) binds both results to the engine commit, candidate run and SHA-256 of each toolset file. The same files are embedded in the WSL rootfs and staged for release downloads.

## Windows

Windows packaging ran on hosted runners and is in the candidate. The installer smoke no longer runs in the candidate workflow ([#130](https://github.com/AitorConS/jerboa/pull/130)); the self-hosted Windows runner lacked the capacity for it.

An earlier v0.55.0 candidate failed that smoke. In the flask-postgres compose example, the PostgreSQL guest timed out under WSL2 because TFS issued a device flush for every storage release. [#131](https://github.com/AitorConS/jerboa/pull/131) batches release cycles: on Linux/QEMU the same stack drops from 62 to 32 device flushes, with fsync durability unchanged. Its effect under WSL2 has not been measured, because the runner lost connection during the retry.

## Publication gate

This directory records validation, not proof of publication. Promotion additionally requires the complete candidate workflow to succeed and the signed inventory to match this native app hash. Published versions are determined by the signed stable manifest at `https://releases.jerboa.dev/channels/stable.json`.
