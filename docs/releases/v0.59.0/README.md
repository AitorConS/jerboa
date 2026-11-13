# Jerboa v0.59.0

Release notes and validation evidence.

Candidate: [43483595008](https://github.com/AitorConS/jerboa/actions/runs/43483595008).

- Engine: `d9bcce4347ee4ad6da111e5d695567c07f3bc08a`
- Desktop: `137fad92699145b754bf1786fc8d72c785e8de96`
- Firecracker macOS: `2464b9872698db426e8d736282a6e118ec11b430`
- Product: `v0.59.0`; Linux kernel toolset: `v0.2.4`, the published one, unchanged.

## What changes

**Firecracker by default.** A daemon that was not told which hypervisor to run now picks Firecracker wherever the machine can run it, and QEMU otherwise.
- **Linux, and Windows inside WSL2:** Firecracker when KVM is available, the `firecracker` binary is installed and the daemon may create network devices. Without KVM the installer now installs QEMU instead of refusing.
- **macOS:** Firecracker on Apple Silicon with macOS 26 or later.
- **It says what it chose.** `jerboa daemon status` prints the hypervisor and what decided it, and Jerboa Desktop shows the same in Settings. Under WSL2 without KVM the reason names the `.wslconfig` setting to change.
- **`jerboa config set hypervisor auto`** returns a machine to the automatic choice.

**Internet access for Firecracker VMs on macOS.** They now reach public IPv4 addresses and resolve external names by default, as QEMU VMs do. The Mac itself and private, shared and link-local networks stay out of reach. `jerboad --fc-security` still replaces the default with your own rules.

**Names inside a network.**
- **Compose:** `service.network` and `alias.network` resolve again on a compose network. They had stopped resolving when compose networks became scoped to their project.
- **Privacy:** a question about a name of the network (an `AAAA` or `MX` record for a service, for instance) is answered locally and no longer forwarded to the external resolver, which could answer with a public domain of the same name.

**Faster pushes.** `jerboa push` uploads packs while it compresses the next ones, at most 24 MiB of them at a time. The gain depends on the registry's latency and has not been measured against registry.jerboa.dev.

**Jerboa Desktop.**
- **macOS: updates from inside the app.** The app follows the signed release manifest, downloads the new version, checks it and replaces itself on restart, putting the previous version back if the new one does not open. This is the first release with it, so it starts working with the update after this one.
- **Windows:** the app enables nested virtualization for WSL2 on Windows 11 before it starts the engine, which Firecracker needs.
- **Moving to Firecracker:** on a machine whose engine is held on QEMU by its configuration and could run Firecracker, the app asks once whether to switch.

**Fixes.**
- A snapshot restore interrupted by a daemon crash left one socket file behind after it was rolled back.
- The release's native macOS validation now runs the network, statistics and snapshot suites, not only the start/stop one.

## Upgrade notes

- **Existing installations keep QEMU until told otherwise.** Releases before this one wrote `hypervisor = "qemu"` into `~/.jerboa/config.toml` without being asked, and that line cannot be told apart from a choice. `jerboa daemon status` says so when Firecracker would run; `jerboa config set hypervisor auto` or the question in Jerboa Desktop switches.
- **Firecracker VMs on macOS now have outbound access.** Until v0.58 they had none unless a policy allowed it. To keep that, start the daemon with a policy file holding `{"version": 1}` (`JERBOA_FIRECRACKER_SECURITY=/path/policy.json jerboa daemon restart`). A policy you already use is unaffected.
- **Windows needs nested virtualization for Firecracker.** It is on by default on Windows 11; Jerboa Desktop and `jerboa daemon install` set it if it was turned off. A WSL2 VM that is already running keeps its setting until `wsl --shutdown`. Windows 10 cannot nest virtualization and stays on QEMU.
- **A Linux daemon without `CAP_NET_ADMIN` picks QEMU.** The systemd unit the installer writes grants it. A daemon started by hand as an ordinary user says why in its log.
- **x86 images on macOS still need QEMU** (`jerboa config set hypervisor qemu`).
- **The Linux kernel toolset is unchanged** (v0.2.4). Existing images and volumes need no change.

## Native Mac

The actual candidate app ZIP passed `scripts/release/native-macos.sh` on a physical Apple Silicon Mac (M5) running macOS 26.6.2. The checkout matched the candidate engine commit. The test used an isolated daemon and temporary VM state.

App: `jerboa-desktop-0.59.0-macos-arm64.zip`.

SHA-256: `a7251662e31df887fcc78008384f3a5f448c4b37f98d09c09ffc5b2b411fada8` (matches `native_macos_app.sha256` in the candidate's signed inventory).

[Machine-readable result](native-macos.json) and [test output](native-macos.log).

From this release the script runs four suites instead of one:

- **Start and stop:** app code-signature integrity, CLI/daemon versions, VM boot with 1 and 4 vCPUs, port forwarding, environment variables, persistent volumes, port conflicts, daemon recovery, restart after guest failure, forced stop.
- **Networks and Compose:** static addresses, service and alias DNS including `name.network` on a compose network, isolation between networks, daemon restart with VMs running, and the outbound policy: refused towards the host by default, allowed where a supplied policy says so.
- **Statistics.**
- **Snapshots:** create, restore in place, races between operations, and recovery from a daemon killed at each phase of a restore (26 checks).

These are functional checks, not throughput benchmarks.

**The Firecracker macOS commit and its native suite.** `experiments/hvf/ci/hvf.sh` passed on this commit and Mac up to its last step (Linux and Debian guests, snapshots, security, I/O limits, 100 network start/stop cycles). In that last step the slirp fuzzer stopped on a signed shift in the vendored libslirp (`0xf << 28` in `ip_icmp.c`), which UBSan reports although compilers produce the intended mask; the fuzzers after it did not run. The same input gives the same report on the commit shipped in v0.58.0, so it is not new in this release. It is fixed in [firecracker-macos #38](https://github.com/AitorConS/firecracker-macos/pull/38), which this release does not include.

**Not exercised:** Jerboa Desktop's new dialogs, its Settings line and the in-app updater were not run in a packaged app; their logic is covered by unit tests and, for the updater's swap, by tests that run the script on real directories.

## Linux kernel

The candidate reused the published toolset v0.2.4 and booted it again under QEMU/KVM 8.2.2 and Firecracker 1.16.0. The [kernel report](kernel-boot.json) is the candidate inventory's `kernel_boot` record, binding both results to the engine commit, candidate run and SHA-256 of each toolset file.

The default-hypervisor choice ran on the KVM runner as an integration test: as an ordinary user without `CAP_NET_ADMIN` the daemon picks QEMU and says why. The case where it picks Firecracker (a daemon with that capability, as the systemd unit grants) was not run on a real host.

## Windows

The candidate's signed Windows installer passed the full installer smoke on a Windows machine started for this run only: silent install, WSL2 distro provisioning from the run's rootfs, and the example unikernels. Whether that engine chose Firecracker or QEMU was not recorded.

## Publication gate

This directory records validation, not proof of publication. Promotion additionally requires the complete candidate workflow to succeed and the signed inventory to match the native app hash recorded here. Published versions are determined by the signed stable manifest at `https://releases.jerboa.dev/channels/stable.json`.
