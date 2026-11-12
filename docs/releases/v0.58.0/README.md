# Jerboa v0.58.0

Release notes and validation evidence.

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
- **Images built before this release keep their kernel.** The kernel is part of the image: rebuild an image to get the faster kernel.
- **`--cpus` above 4 and `--memory` above 2G on macOS** need the VMM of this release; an older VMM keeps the old limits and says so in the error.

## Publication gate

This directory records validation, not proof of publication. Promotion additionally requires the complete candidate workflow to succeed and the signed inventory to match the native app hash recorded here. Published versions are determined by the signed stable manifest at `https://releases.jerboa.dev/channels/stable.json`.
