---
layout: default
title: macOS Apple Silicon
nav_order: 9
---

# macOS Apple Silicon

The native macOS port is a **development preview**. The CLI, daemon, Desktop and filesystem tools run as ARM64 processes on macOS. ARM64 unikernels run directly in QEMU with Apple's Hypervisor.framework (HVF). No Linux host VM, WSL or Rosetta is used. HVF failures are errors; there is no silent TCG fallback.

The port now covers the VM, image, volume, shared-network, DNS, health-check, restart, Compose and observability workflows. Linux-specific mechanisms have explicit native alternatives below; scheduler priority and a sampled memory watchdog are **not equivalent to cgroup isolation**. Firecracker itself still requires Linux/KVM.

The published stable kernel toolset targets Linux/x86_64. Native builds compile their own ARM64 tools. Optional x86 compatibility downloads only signature/checksum-verified guest boot artifacts, keeping all host tools native.

## Build

Use an Apple Silicon Mac with Xcode Command Line Tools, Go 1.25 or newer, Node and pnpm. Install native QEMU and GNU cross-binutils:

```sh
brew install qemu aarch64-elf-binutils
export PATH="/opt/homebrew/bin:$PATH"
./scripts/build-macos.sh --with-x86
python3 scripts/test-macos.py
# Optional downloads and real ARM64 Node/Python runtime tests:
JERBOA_TEST_RUNTIMES=1 python3 scripts/test-macos.py
```

The build fetches the kernel's existing pinned source dependencies. Outputs are under `dist/macos-arm64/`: `jerboa`, `jerboad`, native `mkfs` and `dump`, and the ARM64 kernel. `tools/platform.txt` distinguishes this toolset from old Linux caches. The kernel includes the MMIO `fw_cfg` driver for runtime environment and mount injection and supports the GICv3/V2M combination used by HVF.

The smoke test uses an isolated temporary home and exercises four-vCPU HVF boots, HTTP, environment injection, volume persistence, static IPs, shared networks, guest DNS, TCP/HTTP health checks on unpublished ports, UDP forwarding, host-gateway access, port collision rollback, DNS scope, daemon-crash network recovery, restart exit semantics, Compose, raw disks, native resource accounting and the memory watchdog. When `tools/x86` is installed, it also builds and boots an explicitly emulated x86 image. Optional runtime tests download ARM64 Node and Python packages. Test data and test VMs are cleaned up afterward.

The implementation has been exercised on one Apple Silicon Mac with QEMU 11.1.1. This is not a certification of every M-series chip or macOS version. The build explicitly enables cgo for Apple's native resource-accounting API.

## Start the native engine

```sh
./dist/macos-arm64/jerboa daemon start
./dist/macos-arm64/jerboa status
./dist/macos-arm64/jerboa build examples/hello --platform linux/arm64 --name hello
./dist/macos-arm64/jerboa run hello:latest --attach
./dist/macos-arm64/jerboa daemon stop
```

`daemon start` registers `dev.jerboa.daemon` in the current user's GUI launchd domain. It uses a private Unix socket at `~/.jerboa/jerboad.sock`, shares the CLI/Desktop token in `~/.jerboa/daemon.json`, and logs to `~/.jerboa/jerboad.log`. It requires a logged-in GUI session. `restart` and `stop` operate on that named service, never on a PID from a stale file. This preview does not register automatic startup at login.

The bundled daemon and tools are resolved beside the CLI. QEMU is found on PATH or at `/opt/homebrew/bin/qemu-system-aarch64`. To control a different endpoint, use the regular client commands with `--host`; managed start/stop require the default local endpoint.

Managed launches accept and persist `--metrics-addr`, `--ui-addr`, `--trace-addr` and `--log-format`, just as on Linux. For foreground operation, run `jerboad` directly:

```sh
./dist/macos-arm64/jerboad \
  --tools-dir "$PWD/dist/macos-arm64/tools" \
  --qemu /opt/homebrew/bin/qemu-system-aarch64
```

## Desktop

With sibling `jerboa` and `JerboaDesktop` checkouts:

```sh
cd ../JerboaDesktop
pnpm install
./scripts/stage-macos.sh
pnpm run dist:mac
```

The ARM64 `.app`, DMG and ZIP are written to `dist/`. Desktop bundles the CLI, daemon and kernel tools; QEMU remains an external native prerequisite in this preview. Finder launches use the same native launchd lifecycle as the CLI. The native engine updates with the app, rather than through WSL distro updates.

Local builds have no Developer ID/notarization unless signing credentials are configured. Distribution signing, notarization and a published macOS update feed remain release work. The Windows installer and its WSL lifecycle remain separate.

## Capabilities and current limits

| Capability | macOS implementation / alternative |
| --- | --- |
| ARM64 boot, stop, kill, console/logs | QEMU + HVF; no silent emulation fallback |
| Raw ARM64 disks | Direct `jerboa run /path/disk.img` with the ARM64 kernel |
| x86_64 and legacy images | Explicit `--emulate-x86` uses QEMU TCG; slower than native ARM64 |
| Go / Rust source builds | Linux guest targets selected by architecture; Rust requires its cross-toolchain |
| Python dependencies | manylinux ARM64/x86 wheels selected by target; cache stamp includes architecture |
| Ops runtime packages | Architecture-specific package lookup, SHA-256 and separate ARM64 cache; Node and Python tested |
| Environment and volume mounts | ARM64 MMIO `fw_cfg`; persistent TFS volumes and ephemeral root disks |
| Networks, IPAM and static guest IPs | Private Ethernet switches and gVisor IP stacks, without a kernel TAP driver |
| Guest service DNS | Network-scoped DNS over the virtual gateway; supports the previous preview resolver too |
| Guest outbound networking | Userspace TCP/UDP forwarding; gateway IP reaches host loopback services |
| Published TCP and UDP ports | Host listeners forwarded into the VM's network; bind failures reject the run |
| TCP/HTTP health checks | Direct guest service checks through the userspace stack; no public port required |
| Restart policies | `never`, `always`, `on-failure`, backoff/max retries; PCI pvpanic reports ARM64 failure |
| Compose | Shared networks, service discovery, dependencies, volumes and teardown |
| CPU, memory, disk and network stats | Apple `proc_pid_rusage` + per-VM Ethernet counters; source `darwin-libproc` |
| `--cpu-shares` | Mapped to macOS nice priority (0–19); advisory, not proportional cgroup scheduling |
| `--memory-max` | 100ms RSS watchdog stops a VM exceeding the limit; transient overshoot is possible |
| Disk IOPS/BPS limits | QEMU block throttling |
| Daemon crash recovery | Reconnects surviving QEMU processes to networks, published ports, probes and collectors |
| Metrics / dashboard / tracing | Shared daemon implementation; managed launchd flags supported |
| Firecracker | Use QEMU/HVF locally; use a Linux daemon when Firecracker itself is required |
| Image registries, package stores, signing and cluster RPC | Shared implementation; Go test suite passes, external service deployments not retested here |

### Native alternatives and exact Linux semantics

Native network addresses belong to the userspace stack, so arbitrary macOS host applications cannot route directly to a guest IP. Publish a port to access the service from the host. Guest-to-guest traffic, internal health checks and DNS use the guest addresses directly.

`--cpu-shares` maps larger weights to higher scheduler priority. `--memory-max` observes resident memory and kills the QEMU process when a sample exceeds the requested budget. Both modes appear as warnings in `jerboa inspect`; they do not provide the same resource-isolation guarantees as Linux cgroup v2. The guest's own RAM remains configured by `--memory`.

For exact cgroup semantics or Firecracker, run the existing Linux daemon and connect the native macOS CLI/Desktop to it. An SSH tunnel can expose the authenticated daemon's loopback endpoint without publishing the RPC service:

```sh
ssh -N -L 17890:127.0.0.1:7890 linux-host
# In another terminal, with JERBOA_AUTH_TOKEN set to that daemon's token:
jerboa --host tcp://127.0.0.1:17890 ps
```

The Linux daemon must already listen on `127.0.0.1:7890`. Set Desktop's endpoint override to the tunneled endpoint and configure its matching token. This alternative requires a Linux machine; it does not turn the local ARM64 engine into Firecracker. No remote Linux environment was provisioned in this migration.

### Existing x86 images and packages

```sh
# Fetch optional, signed x86 guest artifacts if they were not bundled:
go run scripts/fetch-x86-tools.go dist/macos-arm64/tools
# Build an x86 Go image, keeping the host CLI/mkfs/daemon ARM64:
jerboa build ./service --platform linux/amd64 --name service-x86
jerboa run service-x86:latest --emulate-x86 -p 8080:8080
```

The fetch command refuses to overwrite an existing `tools/x86` directory. Desktop exposes the emulation choice in the run dialog. VM inspection reports the architecture and emulation mode. Legacy manifests without an architecture require explicit x86 emulation; raw paths are assumed ARM64 unless `--emulate-x86` is set.

Ops runtime availability depends on the upstream catalog. The tested ARM64 packages were Node 20.5.0 and Python 3.10.6. Other versions depend on catalog availability. For a required runtime/version without an ARM package, provide a Linux ARM64 package using the existing package/import workflow (for example, extract it from an ARM64 Docker image), or use its x86 package with explicit emulation. Native Node addons and source-only Python extensions need a Linux target build environment; host macOS binaries cannot be embedded unchanged.

Example HTTP service with persistence:

```toml
[build]
lang = "go"
dirs = ["/data"]
```

```sh
jerboa build ./service --platform linux/arm64 --name service
jerboa volume create service-data --size 64M
jerboa run service:latest -p 127.0.0.1:8080:8080 \
  -e MODE=production -v service-data:/data
```

Guest root-disk writes remain ephemeral; mounted volume writes persist. Before a stable release, validate the supported hardware/macOS matrix, sleep/wake, higher vCPU counts, sustained workloads, upgrades, and signed distribution. Adopted VMs retain the shared Linux implementation’s limitations around historical console capture and restart policies after adoption.

References: [QEMU ARM virt](https://www.qemu.org/docs/master/system/arm/virt.html), [HVF accelerator](https://www.qemu.org/docs/master/system/introduction.html), [fw_cfg protocol](https://www.qemu.org/docs/master/specs/fw_cfg.html), [pvpanic](https://www.qemu.org/docs/master/specs/pvpanic.html), [gvproxy networking](https://github.com/containers/gvisor-tap-vsock), [Apple process accounting](https://github.com/apple-oss-distributions/xnu/blob/main/libsyscall/wrappers/libproc/libproc.h).
