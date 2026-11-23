# Jerboa v0.63.0

Validation evidence. The [changelog entry](../../../CHANGELOG.md#0630---2026-11-22) says what changes and what to do when upgrading.

Candidate: [45090943311](https://github.com/AitorConS/jerboa/actions/runs/45090943311).

- Engine: `83e3d5e02915722f981e7ec8f128dd3dc9c8fb44`
- Desktop: `08498b5f197af1aba239ee315229743211b3369a`
- Spark: `7b2126b9a760e4b2ed6ac47fb14d77ac0173c857`
- Product: `v0.63.0`
- Linux kernel toolset: `v0.2.5`, the published one, unchanged.

## Native Mac

The actual candidate app ZIP passed `scripts/release/native-macos.sh` on a physical Apple Silicon Mac (M5) running macOS 26.6.2. The checkout matched the candidate engine commit. The test used an isolated daemon and temporary VM state.

App: `jerboa-desktop-0.63.0-macos-arm64.zip`.

SHA-256: `0f3ec420c8edd85a65b29830bf4ac5d6c0d6f2b8b644b490f07675f041518b3d`. It matches `native_macos_app.sha256` in the candidate's signed inventory.

- [Machine-readable result](native-macos.json)
- [Test output](native-macos.log)

The script runs four suites: start and stop, networks and Compose, statistics, and snapshots. These are functional checks, not throughput benchmarks. This is the first release whose app ships the macOS runtime as `bin/spark`; the script ran it under that name.

**Not exercised:**
- Jerboa Desktop itself was not opened; only the binaries inside the packaged app were run.
- `experiments/hvf/ci/hvf.sh` of the Spark repository was not run for this release.

## Linux

The candidate ran the full validation on the KVM runner:
- the unit, integration and end-to-end suites;
- the upgrade from the live stable release (v0.62.0) to this build.

It reused the published kernel toolset v0.2.5 and booted it again under QEMU/KVM 8.2.2 and Firecracker 1.16.0. The [kernel report](kernel-boot.json) is the candidate inventory's `kernel_boot` record. It binds both results to the engine commit, the candidate run and the SHA-256 of each toolset file. Every job of this run passed on its first attempt.

**Earlier candidates:** two earlier dispatches for v0.63.0 did not reach promotion.
- [45077121002](https://github.com/AitorConS/jerboa/actions/runs/45077121002) failed `TestFixturesCoverTheLatestRelease`, because there was no v0.62.0 upgrade fixture yet. #337 recorded it.
- [45079294443](https://github.com/AitorConS/jerboa/actions/runs/45079294443): the native macOS job hit `go test`'s 10 minute default while `TestUpgrade_FirstStartInterrupted` replayed the new darwin fixture. Nothing hung. #338 gives that step 30 minutes.

The nightly-only tests (`TestHostDiskFull`, `TestGuardComesBackAfterItIsRemoved`) have not run on this commit.

## Windows

The candidate's Windows installer passed the full installer smoke on a Windows machine started for this run only:
- silent install;
- WSL2 distro provisioning from the run's rootfs;
- the example unikernels.

## Publication gate

This directory records validation, not proof of publication. Promotion also requires two things: the complete candidate workflow must succeed, and the signed inventory must match the native app hash recorded here. The signed stable manifest at `https://releases.jerboa.dev/channels/stable.json` determines which versions are published.
