# Jerboa v0.58.0

Release notes and validation evidence.

Candidate: [43297051915](https://github.com/AitorConS/jerboa/actions/runs/43297051915).

- Engine: `36c996e8d97e374e0d02024a3527921dc46e7d36`
- Desktop: `ac39d202a62f9f0a6ea60c2c4d18cf13c796edab` (unchanged since v0.56.0)
- Firecracker macOS: `f3110ea334f0794eccc3fdd99d0f6ad6f4a1ea78`
- Product: `v0.58.0`; Linux kernel toolset: `v0.2.4`, built by the candidate from the engine commit.

## What changes

**Faster on macOS.**
- **Start-up:** `jerboa run --rm` of a guest that prints one line takes 42 ms on an M5, down from 92 ms. The VMM no longer flushes temporary start-up files to the drive, its supervisor wakes on events instead of every 5 ms, the VMM and its network processes set up their sandboxes at the same time, and the daemon no longer runs the VMM once just to check its config.
- **Published ports:** a VM started without `--network` now uses an MTU of 9000 on its link, like Jerboa networks. Through a published TCP port, iperf3 measures 9.7 Gbit/s into the guest (4.9 before) and 9.0 Gbit/s out of it (2.1 before).
- **Larger guests:** up to 8 vCPUs and 8192 MiB of memory, from 4 and 2048. The daemon asks the VMM for its limits, so a later VMM that raises them needs no Jerboa release.

**Faster guest kernel** (Linux toolset v0.2.4, and the macOS kernel built from the same source).
- **Networking:** TCP checksums are left to virtio devices that offload them, receive buffers are 16 KiB where the device merges buffers, the device is notified once per pass instead of once per packet, and memory is copied with string instructions on x86-64. VM-to-VM TCP on Linux/KVM measured 1.4 Gbit/s before the last three of these and 2.5 Gbit/s after, on a nested-KVM host.
- **Disk:** virtio-blk negotiates `EVENT_IDX`, and a sequential buffered writer starts writeback once it has dirtied 32 MiB instead of leaving everything for `fsync`.

**Pulls and pushes.**
- **Signatures first:** `jerboa pull` checks an image's signatures before it downloads the image, not after.
- **Less work when little changed:** pulling an image whose disk is already stored rebuilds nothing, and the registry's discovery document is cached.
- **More robust:** a read cut by the network is retried; a push whose 15-minute token expires asks for a new one and carries on from what was uploaded; the file that maps image names to digests is replaced atomically.
- **Registries behind Cloudflare Access:** `JERBOA_REGISTRY_ACCESS_HOST`, `JERBOA_REGISTRY_ACCESS_CLIENT_ID` and `JERBOA_REGISTRY_ACCESS_CLIENT_SECRET` send a service token to that registry only.

**Stopping on Linux.** `jerboa stop` under Firecracker on Linux now shuts the guest down gracefully over vsock, as it already did on macOS and under QEMU. A guest kernel without the handler is stopped as before.

**Installer.** The installer waits for `jerboad` to accept connections before it reports it running, so a script can call `jerboa` right after it.

**Packages.** Official image packages from upstream release binaries, and Go programs built from source with a pinned toolchain and patched module versions. Packages are published separately from releases.

**Build.** Go 1.27.4, which fixes GO-2026-6859.

## Upgrade notes

- **macOS VMs outside a network use MTU 9000.** The VMM and the guest are configured together, and only with a VMM that declares support. Nothing to change in images.
- **Start-up no longer survives a power cut cleanly in one case.** If the Mac loses power right after a VM starts, the VMM's temporary directory for that VM may be left behind instead of being deleted on the next start. No image, volume or VM state is involved.
- **Which kernel an image runs.** With Firecracker (the macOS default, and `--hypervisor firecracker` on Linux) every image boots the kernel of the installed release, so existing images get the faster kernel without being rebuilt. With QEMU on Linux x86-64 an image boots the kernel it was built with: rebuild it to get the new one.
- **Existing images and volumes need no change.** An image and a volume made by v0.55.1 (kernel toolset 0.2.2) were used by this release on macOS: the data verified, was rewritten, and was still read by v0.55.1 afterwards. The official `jerboa/redis` and `jerboa/nginx` images, published with toolset 0.2.3, run unchanged.
- **`--cpus` above 4 and `--memory` above 2G on macOS** need the VMM of this release; an older VMM keeps the old limits and says so in the error.

## Native Mac

The actual candidate app ZIP passed `scripts/release/native-macos.sh` on a physical Apple Silicon Mac (M5) running macOS 26.6.2. The checkout matched the candidate engine commit. The test used an isolated daemon and temporary VM state.

App: `jerboa-desktop-0.58.0-macos-arm64.zip`.

SHA-256: `7de13d99331e28178e8819dbbc324bc321eed6a1554ad776dbf2578ba440e3f3` (matches `native_macos_app.sha256` in the candidate's signed inventory).

[Machine-readable result](native-macos.json) and [test output](native-macos.log).

Checks passed: app code-signature integrity, CLI/daemon versions, VM boot with 1 and 4 vCPUs, HTTP/UDP port forwarding with an omitted, loopback and wildcard bind, environment variables, persistent-volume contents across boots, port conflicts, the VMM's declared CPU limit, daemon recovery, guest exit, restart after guest failure, and forced stop. These are functional checks, not throughput benchmarks.

The Firecracker macOS commit has the same tree that passed that repository's native suite (`experiments/hvf/ci/hvf.sh`) on the same Mac: Linux and Debian guests, snapshots, security, I/O limits, 100 network start/stop cycles and three fuzzers. That suite is not part of the hosted CI.

## Linux kernel

The candidate built toolset v0.2.4 from the engine commit. It passed the QEMU/KVM 8.2.2 and Firecracker 1.16.0 boot checks. The [kernel report](kernel-boot.json) is the candidate inventory's `kernel_boot` record, binding both results to the engine commit, candidate run and SHA-256 of each toolset file.

## Windows

The candidate's signed Windows installer passed the full installer smoke on a Windows machine started for this run only: silent install, WSL2 distro provisioning from the run's rootfs, and the example unikernels.

## Publication gate

This directory records validation, not proof of publication. Promotion additionally requires the complete candidate workflow to succeed and the signed inventory to match the native app hash recorded here. Published versions are determined by the signed stable manifest at `https://releases.jerboa.dev/channels/stable.json`.
