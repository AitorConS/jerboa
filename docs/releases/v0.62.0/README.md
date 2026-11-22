# Jerboa v0.62.0

Validation evidence. The [changelog entry](../../../CHANGELOG.md#0620---2026-11-22) says what changes and what to do when upgrading.

Candidate: [44967249215](https://github.com/AitorConS/jerboa/actions/runs/44967249215).

- Engine: `d9299e34bd8041a060e6ad042638a12dcc62d651`
- Desktop: `5cd6f03309cd613ad87055fc0a563fd2e311ec33`
- Firecracker macOS: `f5a4916625121f867e64c738f38a22fa7bc61d80`
- Product: `v0.62.0`
- Linux kernel toolset: `v0.2.5`, built from this revision (it carries the TFS append headroom of #328).

## Native Mac

The actual candidate app ZIP passed `scripts/release/native-macos.sh` on a physical Apple Silicon Mac (M5) running macOS 26.6.2. The checkout matched the candidate engine commit. The test used an isolated daemon and temporary VM state.

App: `jerboa-desktop-0.62.0-macos-arm64.zip`.

SHA-256: `7c2a19dc2c4bb1f6ce5c9307438e7c8ee54af703975ef1cd7a0ecd66b30b7263`. It matches `native_macos_app.sha256` in the candidate's signed inventory.

- [Machine-readable result](native-macos.json)
- [Test output](native-macos.log)

The script runs four suites: start and stop, networks and Compose, statistics, and snapshots. These are functional checks, not throughput benchmarks.

Before the release, the guest kernel change of #328 was also checked on this Mac:
- the storage guest tests of `tests/guest`, with the new `concurrent_append.c`;
- a full benchmark against Docker Desktop.

The guest tests that passed:
- disk pressure;
- random write;
- large write;
- truncate and truncate at ENOSPC;
- extend gap;
- uninit integrity;
- map ENOSPC;
- mapped page-cache pressure;
- concurrent append.

All of them pass, with their restart passes, except `reuse_enospc`. That test fails its round-2 assertion with the v0.61.0 kernel as well.

**Not exercised:**
- Jerboa Desktop itself was not opened; only the binaries inside the packaged app were run.
- `experiments/hvf/ci/hvf.sh` of the Firecracker macOS repository was not run for this release.

## Linux

The candidate ran the full validation on the KVM runner:
- the unit, integration and end-to-end suites;
- the upgrade from the live stable release (v0.61.0) to this build.

It built the kernel toolset v0.2.5 from this revision and booted it under QEMU/KVM 8.2.2 and Firecracker 1.16.0. The [kernel report](kernel-boot.json) is the candidate inventory's `kernel_boot` record. It binds both results to the engine commit, the candidate run and the SHA-256 of each toolset file. Every job passed on its first attempt.

The nightly-only tests (`TestHostDiskFull`, `TestGuardComesBackAfterItIsRemoved`) have not run on this commit.

## Windows

The candidate's Windows installer passed the full installer smoke on a Windows machine started for this run only:
- silent install;
- WSL2 distro provisioning from the run's rootfs;
- the example unikernels.

## Publication gate

This directory records validation, not proof of publication. Promotion also requires two things: the complete candidate workflow must succeed, and the signed inventory must match the native app hash recorded here. The signed stable manifest at `https://releases.jerboa.dev/channels/stable.json` determines which versions are published.
