---
layout: default
title: Firecracker shutdown on macOS
nav_order: 14
---

# Firecracker shutdown on macOS

Jerboa requests a guest-visible VirtIO input `KEY_POWER` event when stopping a
Nanos ELF guest under the macOS Firecracker/HVF backend. The Nanos driver
re-arms all four event buffers before scheduling `kernel_powerdown()` outside
interrupt context. Nanos then delivers `SIGTERM` to guest processes and runs
its normal kernel shutdown path, including `storage_sync()`, before powering
off.

The VMM waits up to 35 seconds for guest exit and then explicitly force-stops a
non-cooperative guest. Jerboa keeps a separate 40-second outer process-kill
bound so a broken control plane cannot leave the daemon's stop request hanging.
`jerboa stop --force` continues to terminate immediately.

These guarantees have an important boundary: kernel synchronization covers
writes already handed to the kernel (including dirty page-cache data), not
bytes still held only in an application's userspace buffer. Applications that
need their own transactional or protocol-level cleanup must handle `SIGTERM`
and flush that state. A forced timeout is not a clean shutdown and must not be
described as an fsync guarantee.

The reproducible macOS acceptance is:

```sh
python3 scripts/test-firecracker-shutdown.py
```

Override `JERBOA_SHUTDOWN_FC` to select another compatible Firecracker binary.
The test creates an isolated temporary HOME, daemon sockets, dynamic ports,
volumes and VMs. It covers a normally terminating application, an application
that ignores `SIGTERM` for the full Nanos grace period, persistence after a new
boot, immediate force-stop, restart suppression after explicit stop, and a
kernel without the power driver that exercises the 35-second fallback.
