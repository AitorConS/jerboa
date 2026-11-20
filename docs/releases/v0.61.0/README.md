# Jerboa v0.61.0

Validation evidence. The [changelog entry](../../../CHANGELOG.md#0610---2026-11-20) says what changes and what to do when upgrading.

Candidate: [44809909545](https://github.com/AitorConS/jerboa/actions/runs/44809909545).

- Engine: `5bf18a1c634b3701082734bd83ab9c6eea989798`
- Desktop: `5cd6f03309cd613ad87055fc0a563fd2e311ec33`
- Firecracker macOS: `f5a4916625121f867e64c738f38a22fa7bc61d80`
- Product: `v0.61.0`
- Linux kernel toolset: `v0.2.4`, the published one, unchanged.

## Native Mac

The actual candidate app ZIP passed `scripts/release/native-macos.sh` on a physical Apple Silicon Mac (M5) running macOS 26.6.2. The checkout matched the candidate engine commit. The test used an isolated daemon and temporary VM state.

App: `jerboa-desktop-0.61.0-macos-arm64.zip`.

SHA-256: `cd629f89e26d101910a0f81ee3d367b65f59b4ca3772ed655c163ebff9fd8404`. It matches `native_macos_app.sha256` in the candidate's signed inventory.

- [Machine-readable result](native-macos.json)
- [Test output](native-macos.log)

The script runs four suites: start and stop, networks and Compose, statistics, and snapshots. These are functional checks, not throughput benchmarks.

**Not exercised:**
- Jerboa Desktop itself was not opened; only the binaries inside the packaged app were run.
- `experiments/hvf/ci/hvf.sh` of the Firecracker macOS repository was not run for this release.

## Linux

The candidate ran the full validation on the KVM runner:
- the unit, integration and end-to-end suites;
- the upgrade from the live stable release (v0.60.0) to this build.

It reused the published kernel toolset v0.2.4 and booted it again under QEMU/KVM 8.2.2 and Firecracker 1.16.0. The [kernel report](kernel-boot.json) is the candidate inventory's `kernel_boot` record. It binds both results to the engine commit, the candidate run and the SHA-256 of each toolset file.

**Kernel boot job:** its first attempt failed before booting anything. The KVM runner's job-started guard (`scripts/release/kvm-runner-guard.py`), changed in #316, had not been reinstalled on the runner yet. After it was reinstalled from main, the re-run passed `check-installed` and both boots.

The nightly-only tests (`TestHostDiskFull`, `TestGuardComesBackAfterItIsRemoved`) have not run on this commit.

## Windows

The candidate's Windows installer passed the full installer smoke on a Windows machine started for this run only:
- silent install;
- WSL2 distro provisioning from the run's rootfs;
- the example unikernels.

## Publication gate

This directory records validation, not proof of publication. Promotion also requires two things: the complete candidate workflow must succeed, and the signed inventory must match the native app hash recorded here. The signed stable manifest at `https://releases.jerboa.dev/channels/stable.json` determines which versions are published.
