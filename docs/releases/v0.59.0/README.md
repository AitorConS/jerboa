# Jerboa v0.59.0

Release notes and validation evidence.

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
