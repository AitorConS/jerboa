# Jerboa v0.60.0

Validation evidence. What changes and what to do when upgrading is in the [changelog entry](../../../CHANGELOG.md#0600---2026-11-18).

Candidate: [44332997525](https://github.com/AitorConS/jerboa/actions/runs/44332997525).

- Engine: `157a762ba8ae12a94148526ae1c7d2babee38dbe`
- Desktop: `6a1df59b4dd944fa08310b2fbf9e6405c74a50cc`
- Firecracker macOS: `c4a5f2e5372665a7096ebe360772a46e3005f99f`
- Product: `v0.60.0`; Linux kernel toolset: `v0.2.4`, the published one, unchanged.

## Native Mac

The actual candidate app ZIP passed `scripts/release/native-macos.sh` on a physical Apple Silicon Mac (M5) running macOS 26.6.2. The checkout matched the candidate engine commit. The test used an isolated daemon and temporary VM state.

App: `jerboa-desktop-0.60.0-macos-arm64.zip`.

SHA-256: `b462d6306ad82b35625a6c0e36119cec72239a11237ef612907bfbba32cae63b` (matches `native_macos_app.sha256` in the candidate's signed inventory).

[Machine-readable result](native-macos.json) and [test output](native-macos.log).

The script runs four suites: start and stop, networks and Compose, statistics, and snapshots (26 checks). These are functional checks, not throughput benchmarks.

**Not exercised:** Jerboa Desktop itself was not opened; only the binaries inside the packaged app were run. `experiments/hvf/ci/hvf.sh` of the Firecracker macOS repository was not run for this release.

## Linux

The candidate ran the full validation on the KVM runner: unit, integration and end-to-end suites, and the upgrade from the live stable release (v0.59.0) to this build.

It reused the published kernel toolset v0.2.4 and booted it again under QEMU/KVM 8.2.2 and Firecracker 1.16.0. The [kernel report](kernel-boot.json) is the candidate inventory's `kernel_boot` record, binding both results to the engine commit, candidate run and SHA-256 of each toolset file.

The nightly-only tests (`TestHostDiskFull`, `TestGuardComesBackAfterItIsRemoved`) have not run on this commit.

## Windows

The candidate's signed Windows installer passed the full installer smoke on a Windows machine started for this run only: silent install, WSL2 distro provisioning from the run's rootfs, and the example unikernels.

It passed on the second attempt. In the first, `go build` of the `hello` example failed on the Windows host with `The supplied user buffer is not valid for the requested operation` while writing to its temporary directory; the same step had passed on the two earlier candidates of this version, and passed when the job was run again on a new machine with the same installer.

## Publication gate

This directory records validation, not proof of publication. Promotion additionally requires the complete candidate workflow to succeed and the signed inventory to match the native app hash recorded here. Published versions are determined by the signed stable manifest at `https://releases.jerboa.dev/channels/stable.json`.
