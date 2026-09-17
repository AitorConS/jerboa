---
layout: default
title: Using Jerboa on macOS
nav_order: 9
---

# Using Jerboa on macOS
{: .no_toc }

Jerboa runs natively on Apple Silicon Macs. The CLI, the daemon and your VMs
run directly on macOS through Apple's Hypervisor.framework — there is no Linux
VM in between, no Docker Desktop and no Rosetta.

macOS support is a **development preview**. Most everyday workflows (build,
run, networks, volumes, Compose, logs and stats) work the same as on Linux, and
this page explains where the Mac behaves differently.

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

## Requirements

- A Mac with Apple Silicon (M1 or newer).
- macOS 26 or newer.

Intel Macs are not supported.

## Install

1. Download the Jerboa Desktop DMG for macOS from the release page
   (`jerboa-desktop-VERSION-macos-arm64.dmg`).
2. Open the DMG and drag **Jerboa Desktop** into **Applications**. If you open
   the app from anywhere else, it asks to move itself there, and it will not
   start the engine until it runs from Applications.
3. Open the app. Because the preview is not yet signed or notarized by Apple,
   macOS shows *"Jerboa Desktop" Not Opened*. Click **Done**, go to
   **System Settings → Privacy & Security**, scroll down and click
   **Open Anyway** next to the Jerboa Desktop message, confirm with your
   password, and open the app again. You only need to do this once per
   downloaded version.
4. When the engine starts for the first time, the app installs the `jerboa` and
   `jerboad` commands in `/usr/local/bin` so you can use Jerboa from the
   terminal. macOS asks for your administrator password. Existing commands that
   the app did not create (for example, from Homebrew) are left untouched.

Check the installation from a terminal:

```sh
jerboa status
jerboa version
```

The app is self-contained: it includes the CLI, the daemon, the kernel tools
and the Firecracker runtime. You do not need Homebrew, QEMU or Go to run
images.

The `install.sh` script does not support macOS yet — use the DMG.

### Updating

Download the newer DMG and replace the app in Applications. The engine is
updated together with the app. Stop your running VMs first.

## The daemon

The Desktop app starts the daemon for you. From the terminal you can manage it
too:

```sh
jerboa daemon start
jerboa daemon status
jerboa daemon restart
jerboa daemon stop
```

- The daemon runs as your user (no `sudo`) and only while you are logged in.
- It does not start automatically at login yet; open the app or run
  `jerboa daemon start`. By default the app starts the engine when it opens
  (you can turn this off in **Settings**).
- Its log is at `~/.jerboa/jerboad.log`.
- Your data (images, volumes, snapshots and settings) lives in `~/.jerboa`.

## Choosing an engine: Firecracker or QEMU

Jerboa on macOS can run VMs with two engines:

| | Firecracker (default in the app) | QEMU (optional) |
| --- | --- | --- |
| Included in the app | Yes | No — install with `brew install qemu` |
| Boot speed and footprint | Fastest, smallest | Slower, heavier |
| ARM64 images | Yes | Yes |
| x86_64 images (emulated) | No | Yes, with `--emulate-x86` |
| Snapshots | Yes | No |
| Outbound internet from VMs | Blocked unless you allow it | Allowed |
| `--cpu-shares` / `--memory-max` | Not supported | Approximate |
| vCPUs / RAM per VM | 1–4 vCPUs, 64–2048 MiB | No fixed range |

Use Firecracker unless you need to run x86 images. To switch engines, stop your
VMs first. Then, in the Desktop app, open **Settings → Engine configuration**,
choose the **Hypervisor**, click **Save** and then **Restart engine**. From the
terminal:

```sh
jerboa config set hypervisor qemu          # or: firecracker
jerboa daemon restart
```

Both write the same setting to `~/.jerboa/config.toml`, so the app and the CLI
always agree.

**Tip:** when the setting is left at its default, the app starts the engine with
Firecracker but `jerboa daemon start` uses QEMU. If you start the daemon from
the terminal, set the engine explicitly once with
`jerboa config set hypervisor firecracker`.

## Building images for the Mac

Your Mac runs **ARM64 Linux** unikernels. `jerboa build` targets ARM64
automatically on macOS, and packages such as the Node and Python runtimes are
downloaded in their ARM64 variant.

```sh
jerboa init
jerboa build . --name hello
jerboa run hello:latest -p 127.0.0.1:8080:8080
```

### Use Linux binaries, not macOS binaries

A unikernel runs a **Linux** program. A binary you compiled for macOS will not
boot, and Jerboa stops the build with an error such as:

```text
myapp is a macOS Mach-O executable, not a Linux ELF binary
```

Either point `jerboa build` at the source directory (Jerboa cross-compiles Go,
Rust, Node and Python projects for you), or build a Linux ARM64 binary yourself:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o myapp .
jerboa build ./myapp --name myapp
```

Node native addons and Python extensions without ARM64 Linux wheels also need to
be built for Linux; macOS builds of them cannot be copied into the image.

### When a package has no ARM64 version

Not every package in the catalog is available for ARM64. For example, the
PostgreSQL packages in the ops catalog are x86_64 only. You have three options:

- **Create your own ARM64 package** from a Linux ARM64 binary, with no Docker
  needed:

  ```sh
  jerboa pkg create myservice:1.0.0 ./myservice --platform linux/arm64
  ```

  You can also import one from an ARM64 Docker image with
  `jerboa pkg from-docker ... --platform linux/arm64`. See
  [Linux ARM64 packages]({% link packages-arm64.md %}).
- **Run the x86 version with emulation** (QEMU only, see below).
- **Run it on a Linux machine** and connect your Mac to it (see
  [Using a Linux daemon from your Mac](#using-a-linux-daemon-from-your-mac)).

### Running x86_64 images

Existing x86 images can run on the Mac through emulation. This needs the QEMU
engine and is noticeably slower than native ARM64:

```sh
brew install qemu
jerboa config set hypervisor qemu && jerboa daemon restart
jerboa build ./service --platform linux/amd64 --name service-x86
jerboa run service-x86:latest --emulate-x86 -p 8080:8080
```

With Firecracker, `--emulate-x86` fails with
`firecracker/HVF cannot emulate x86 images`. Rebuild the image for
`linux/arm64`, or switch to QEMU. In the Desktop app, the run dialog shows an
**Emulate x86 image** option for x86 images; it is only available when the
hypervisor is set to QEMU.

## Networking and ports

Networking on the Mac is simpler than on Linux:

- **`-p` works without `--network`.** Every VM gets a private network by
  default, so you can publish ports straight away.
- **TCP and UDP** ports can both be published.
- A published port listens on **all interfaces** by default. Use
  `127.0.0.1:HOST:GUEST` to keep it local to your Mac.

```sh
jerboa run web:latest -p 127.0.0.1:8080:80
jerboa run dns:latest -p 5353:53/udp
```

Create a named network when VMs need to talk to each other. VMs on the same
network reach each other by name, and each VM gets an IP from the network's
subnet:

```sh
jerboa network create app --subnet 172.25.0.0/24
jerboa run postgres-arm64:latest --name db --network app --health-check tcp:5432
jerboa run backend:latest --network app -e DATABASE_HOST=db -p 127.0.0.1:8080:8080
```

Things to know:

- **Your Mac cannot reach a VM's IP directly.** Publish a port and use
  `localhost` instead. VMs reach each other by IP or name without restrictions.
- **A VM can reach services running on your Mac** through its network gateway
  address (the first address of a named network's subnet, or `10.0.2.2` on the
  default private network), which maps to your Mac's `localhost`.
- A VM can join one network at a time.
- With Firecracker, health checks on a VM without `--network` need a published
  TCP port. On a named network, health checks work on any port.

### Allowing internet access (Firecracker)

For safety, Firecracker VMs **cannot open outbound connections or resolve
external DNS names** unless you allow it. Traffic inside a named network and
published ports always work.

Write a policy file listing the destinations your VMs need:

```json
{
  "version": 1,
  "egress": [{"protocol": "tcp", "address": "203.0.113.10", "port": 443}],
  "dns": {"address": "1.1.1.1", "port": 53}
}
```

Then restart the daemon with it:

```sh
JERBOA_FIRECRACKER_SECURITY=~/jerboa-policy.json jerboa daemon restart
```

Each rule allows one exact IPv4 address, protocol and port. Allowing DNS does
not allow connections to the addresses it returns — add those as `egress`
rules too. To let a VM reach a service on your Mac through the gateway, allow
`127.0.0.1` and that port. The policy applies to all VMs of the daemon, and you
need to pass the variable again each time you start the daemon from the terminal.

QEMU VMs are not restricted by this policy.

## Volumes and disks

Volumes work as on Linux and keep their data between runs. Everything a VM
writes outside a volume is lost when it stops.

```sh
jerboa volume create service-data --size 64M
jerboa run service:latest -v service-data:/data -p 127.0.0.1:8080:8080
```

With Firecracker a VM can mount up to three volumes. Disk IOPS and throughput
limits are supported by both engines.

## Stopping VMs

`jerboa stop` asks the guest to shut down, like pressing a power button. Your
application receives `SIGTERM` and data already written to files is flushed to
disk. If the application does not exit within about 35 seconds, the VM is
forced off.

- Handle `SIGTERM` in your application if it keeps data in its own memory
  buffers (for example, a database that needs to checkpoint).
- `jerboa stop --force` stops the VM immediately and does not guarantee that
  recent writes reach the disk.

## Resource limits

The Mac has no Linux cgroups, so CPU and memory limits behave differently:

| Flag | QEMU | Firecracker |
| --- | --- | --- |
| `--cpus`, `--memory` | Supported | 1–4 vCPUs, 64–2048 MiB |
| `--cpu-shares` | Lower or higher process priority; not a hard share | Rejected |
| `--memory-max` | The VM is stopped if it exceeds the limit (checked every 100 ms) | Rejected |
| Disk IOPS/BPS limits | Supported | Supported |

`jerboa inspect` shows a warning when a limit is only approximate.

## Snapshots

With Firecracker you can save a running VM and later resume it exactly where it
was — memory, running processes and root disk included. This is handy for
skipping a slow start-up during local development.

```sh
jerboa snapshot create web warm-cache     # pause, save, resume
jerboa stop web
jerboa snapshot restore web warm-cache    # resume the stopped VM from the snapshot
jerboa snapshot ls
jerboa snapshot inspect warm-cache
jerboa snapshot rm warm-cache
```

Requirements and limits:

- Firecracker only, and the VM must be on a **named network** (`--network`).
- VMs with **volumes** cannot be snapshotted.
- A snapshot restores **the same VM** it was taken from, while that VM is
  stopped. It is not a way to clone VMs or move them to another Mac.
- The VM keeps its name, IP and published ports. Open TCP connections are
  dropped, so clients must reconnect.
- The VM is paused while the snapshot is taken.
- A snapshot is about as large as the VM's RAM plus its root disk. By default
  you can keep up to 16 snapshots and 32 GiB in total.
- Snapshots are stored in `~/.jerboa/snapshots` and contain the VM's memory,
  including environment variables and secrets. Treat them as sensitive.
- Snapshots are for local development, not backups.

More detail: [Firecracker snapshots on macOS]({% link macos-snapshots.md %}).

## Stats

`jerboa stats` and the Desktop app show CPU, memory, disk and network usage for
each VM. On the Mac these numbers are measured from the host:

- **CPU**: 100% equals one full core of your Mac.
- **Memory**: how much of your Mac's RAM the VM is actually using. It is not the
  `--memory` value you configured, nor the memory used inside the guest.
- **Network**: bytes sent and received by the VM, on both named networks and
  the default private network.

## Compose

Compose stacks work on the Mac, including networks, service discovery, health
checks, dependencies and volumes. Services can set a fixed IP and aliases:

```yaml
services:
  postgres:
    image: postgres-arm64:latest
    health_check: tcp:5432
    networks:
      app:
        ipv4_address: 172.25.0.10
        aliases: [database]
  backend:
    image: backend-arm64:latest
    depends_on: [postgres]
    networks: [app]
    environment: [DATABASE_HOST=database, DATABASE_PORT=5432]
    ports: ['127.0.0.1:8080:8080']
networks:
  app:
    ipam:
      config:
        - subnet: 172.25.0.0/24
```

Every image in the stack must be ARM64 (or run with QEMU emulation). Each
service can join one network.

## Using a Linux daemon from your Mac

If you need exact Linux behavior — cgroup resource isolation, upstream
Firecracker or x86 images at full speed — run `jerboad` on a Linux machine and
use your Mac's CLI or Desktop app as the client. An SSH tunnel keeps the daemon
private:

```sh
ssh -N -L 17890:127.0.0.1:7890 linux-host
# In another terminal, with JERBOA_AUTH_TOKEN set to the Linux daemon's token:
jerboa --host tcp://127.0.0.1:17890 ps
```

To use the tunnel from the Desktop app, set **Settings → Endpoint override** to
`tcp://127.0.0.1:17890` and add the Linux daemon's token to
`~/.jerboa/config.toml`:

```toml
[daemon]
token = "<linux daemon token>"
```

Clear the endpoint override to return to the local engine.

## Known limitations

- Preview quality: tested on a limited set of Macs and macOS versions.
- The app is not signed or notarized yet, and there is no `install.sh` or
  Homebrew install for macOS.
- The daemon does not start automatically at login.
- Your Mac cannot connect to VM IPs directly; use published ports.
- One network per VM.
- Firecracker blocks outbound traffic by default and cannot emulate x86.
- CPU and memory limits are approximate (QEMU) or unavailable (Firecracker).
- After a daemon crash, running VMs are recovered, but their earlier console
  output is not, and their restart policy no longer applies.

For errors you may hit on the Mac, see
[Troubleshooting]({% link troubleshooting.md %}#macos-errors). To build Jerboa
for macOS from source, see [macOS Apple Silicon]({% link macos.md %}) and
[Firecracker on macOS]({% link macos-firecracker.md %}).
