# Jerboa v0.54.3 validation evidence

Candidate: [36049371438](https://github.com/AitorConS/jerboa/actions/runs/36049371438).

- Engine: `5b330ce171b62d107b83ce9e5c341744bd023ba2`
- Desktop: `a9c31b938ddd06ec9ec8ff0cd259a9bb4b398f86`
- Firecracker macOS: `1e704adb9b51cf6bf1d29bb8e3c793ef3054f0fb`
- Product: `v0.54.3`; source-built Linux kernel toolset: `v0.2.1`.

## Native Mac

The actual candidate app ZIP passed `scripts/release/native-macos.sh` on a physical Apple Silicon Mac running macOS 26.6.2. The checkout matched the candidate engine commit. The test used an isolated daemon and temporary VM state.

App: `jerboa-desktop-0.54.3-macos-arm64.zip`.

SHA-256: `ab7c6f7f6539709ace5070237b7fab616b2b1073712f7035cf864b58b4402458`.

[Machine-readable result](native-macos.json) and [test output](native-macos.log).

Checks passed: app code-signature integrity, CLI/daemon versions, VM boot with 1 and 4 vCPUs, HTTP/UDP port forwarding, environment variables, persistent-volume contents across boots, port conflicts, daemon recovery, guest exit, restart after guest failure, and forced stop. These are functional checks, not throughput benchmarks.

## Linux kernel

The candidate's exact staged toolset passed QEMU/KVM 8.2.2 and Firecracker 1.16.0 boot checks with two vCPUs. [Kernel report](kernel-boot.json) binds both results to the engine commit, candidate run and SHA-256 of each toolset file. The same files are embedded in the WSL rootfs and staged for release downloads.

The initial candidate was cancelled after detecting an uninitialized PVH stack. [PR #102](https://github.com/AitorConS/jerboa/pull/102) fixes that bootstrap ordering; this candidate includes the fix. The next candidate exposed a Windows volume-routing regression; [PR #104](https://github.com/AitorConS/jerboa/pull/104) makes every volume command resolve the WSL daemon and removes swallowed setup errors from the installer test. This final candidate includes both fixes and has fresh native evidence.

## Publication gate

This directory records validation, not proof of publication. Promotion additionally requires the complete candidate workflow to succeed, including Windows installation and guest checks, and the signed inventory to match this native app hash. Published versions are determined by the signed stable manifest at `https://releases.jerboa.dev/channels/stable.json`.
