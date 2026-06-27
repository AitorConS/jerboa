---
layout: default
title: Getting Started
nav_order: 2
---

# Getting Started
{: .no_toc }

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

## Prerequisites

### Required

| Dependency | Version | Notes |
|---|---|---|
| QEMU | 7.0+ | `qemu-system-x86_64` must be in PATH |
| Linux kernel | 5.4+ | KVM acceleration (`/dev/kvm`) |
| Go | 1.25+ | Required only when building `jerboa`/`jerboad` from source |

### Optional

| Dependency | Notes |
|---|---|
| Cross-compiler (`x86_64-elf-gcc`, `nasm`) | Only needed to build the Nanos kernel from source |

{: .note }
Windows is supported for development but **KVM is not available**. VMs run via software emulation (slow). For production use, run on a Linux host with `/dev/kvm`.

### Install QEMU

**Ubuntu / Debian**
```bash
sudo apt-get install qemu-system-x86
```

**Fedora / RHEL**
```bash
sudo dnf install qemu-system-x86
```

**macOS**
```bash
brew install qemu
```

**Windows**
```powershell
winget install SoftwareFreedomConservancy.QEMU
```

### Enable KVM (Linux)

```bash
# Check KVM is available
ls -la /dev/kvm

# Add your user to the kvm group if needed
sudo usermod -aG kvm $USER
# Log out and back in for the group to take effect
```

---

## Installation

### Download pre-built binaries

Download the latest release from [GitHub Releases](https://github.com/AitorConS/jerboa/releases/tag/latest):

| Platform | Binary |
|---|---|
| Linux amd64 | `jerboa-linux-amd64`, `jerboad-linux-amd64` |
| Linux arm64 | `jerboa-linux-arm64`, `jerboad-linux-arm64` |
| Windows amd64 | `jerboa-windows-amd64.exe`, `jerboad-windows-amd64.exe` |

```bash
# Linux — download and install
curl -Lo /usr/local/bin/jerboa   https://github.com/AitorConS/jerboa/releases/latest/download/jerboa-linux-amd64
curl -Lo /usr/local/bin/jerboad  https://github.com/AitorConS/jerboa/releases/latest/download/jerboad-linux-amd64
chmod +x /usr/local/bin/jerboa /usr/local/bin/jerboad
```

### Linux: one-shot install (recommended)

On a Linux host, `scripts/install.sh` provisions everything the daemon needs —
firecracker, qemu, runtime deps, a dedicated unprivileged `jerboa` user, and a
systemd service — and points your CLI at it. It is the Linux counterpart to
`jerboa daemon install` on Windows (which imports a preconfigured WSL2 distro).

```bash
sudo scripts/install.sh
jerboa status        # daemon should report running
```

The daemon starts automatically and on boot (`systemctl status jerboad`,
`journalctl -u jerboad -f`). On Windows the `jerboa daemon` command group manages
the WSL2 distro instead; it is hidden on Linux, where jerboad runs natively.

### Build from source

```bash
git clone https://github.com/AitorConS/jerboa.git
cd jerboa
make build
# Produces: dist/jerboa  dist/jerboad
```

---

## Quick Start

### 1. Start the daemon

The daemon (`jerboad`) must run in the background before you can use the `jerboa` CLI.

```bash
# Linux (listens on /var/run/jerboad.sock)
sudo jerboad --qemu qemu-system-x86_64

# Without sudo (custom socket path)
jerboad --socket /tmp/jerboad.sock --qemu qemu-system-x86_64

# With observability enabled — Prometheus metrics + web dashboard
jerboad --socket /tmp/jerboad.sock --metrics-addr :9090 --ui-addr :8080
```

{: .note }
See [Observability]({% link observability.md %}) for the full set of daemon flags that enable metrics, tracing, structured logging, the dashboard, and the SQLite VM store.

Keep this terminal open, or run as a background service (see [Running as a Service](#running-as-a-service)).

### 2. Build your first image

You need a **static Linux ELF binary** — compiled for `GOOS=linux` with no dynamic library dependencies.

**Example: Go hello world**

```go
// hello.go
package main

import "fmt"

func main() {
    fmt.Println("Hello from unikernel!")
}
```

**Linux / macOS:**
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o hello hello.go
```

**Windows (PowerShell):**
```powershell
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -o hello-linux hello.go
```

**Build the unikernel image:**

```bash
# Linux / macOS
jerboa build ./hello --name hello
# sha256:abc123...  hello:latest
```

```powershell
# Windows
.\jerboa-windows-amd64.exe build hello-linux --name hello
# sha256:abc123...  hello:latest
```

On first run, `jerboa build` automatically downloads `mkfs`, `kernel.img`, and `boot.img` from the latest release into `~/.jerboa/tools/` (or `%USERPROFILE%\.jerboa\tools\` on Windows). On Windows, the build step runs through WSL2.

If a newer kernel version is available, `jerboa build` will prompt before building:

```
⚠  New kernel version available: v0.1.1 (installed: v0.1.0)
Update kernel before building? [y/N]
```

You can also manage the kernel tools explicitly — see [Kernel Commands]({% link cli-reference.md %}#kernel-commands) in the CLI reference.

### 2b. Or build directly from source

For Go, Node.js, Python, and Rust projects, `jerboa build` can skip the manual cross-compilation step entirely: point it at a source directory and it detects the language from project markers (`go.mod`, `package.json`, `pyproject.toml`/`requirements.txt`, `Cargo.toml`), compiles it, resolves a matching language runtime, and packages everything into one image.

**Example: a Node.js app**

```js
// hi.js
console.log("Hello from jerboa+node!");
```

```json
// package.json — "engines.node" pins the runtime's major version
{
  "name": "myapp",
  "version": "1.0.0",
  "main": "hi.js",
  "engines": { "node": "11" }
}
```

```bash
# The Node.js driver reads "engines.node", resolves the closest matching
# runtime from the nanovms/ops ecosystem (here: eyberg/node:v11.x), and
# bundles the node binary together with your script
jerboa build ./myapp --name myapp --pkg-source ops
jerboa run myapp:latest --attach
# Hello from jerboa+node!
```

Without `engines.node`, the driver defaults to Node 20. The same pattern applies to the other drivers:

| Language | Detected by | What happens |
|---|---|---|
| `go` | `go.mod` | Compiled with `CGO_ENABLED=0` into a static ELF binary — no runtime package needed |
| `node` | `package.json` | `npm install --production`, then bundled with a `node` runtime (version from `engines.node`, default `20`) |
| `python` | `pyproject.toml` / `requirements.txt` | Dependencies installed with `pip`, bundled with a `python` runtime (version from `requires-python`) |
| `rust` | `Cargo.toml` | Compiled with `cargo build --release` against a musl target into a static binary |

You can force a specific driver with `--lang go|node|python|rust`, cross-compile with `--platform linux/arm64`, and configure builds declaratively (including multi-stage builds) with a `unikernel.toml` file. See [`jerboa build`]({% link cli-reference.md %}#jerboa-build) for the complete flag reference and `unikernel.toml` format.

{: .important }
Building an HTTP server (Flask, Express, Next.js, ...)? Pass `--port <port>` to `jerboa build` so Nanos brings up its network stack at boot — without it the unikernel has no network and the server can't bind. See [Networking & Environment in the Image Manifest]({% link cli-reference.md %}#networking-environment-in-the-image-manifest).

**Framework examples:** the repo's `examples/` directory includes a minimal Flask app (`examples/flaskapp`, no `unikernel.toml` needed) and a Next.js app using `output: "standalone"` (`examples/nextapp`, using `[build] run` and `.unignore` — see [Framework Build Steps]({% link cli-reference.md %}#framework-build-steps)):

```bash
# Flask
jerboa build examples/flaskapp --pkg-source ops --pkg eyberg/python:3.10.6 --port 8080 --name flaskapp
jerboa run flaskapp -p 8080:8080

# Next.js
jerboa build examples/nextapp --pkg-source ops --port 3000 --name nextapp
jerboa run nextapp -p 3000:3000
```

### 3. Run it

```bash
jerboa run hello:latest
# a3f8c2d1-...

# Check it's running
jerboa ps
# ID                                    NAME  STATE    HEALTH   IMAGE
# a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f  -     running  unknown  hello:latest

# Read the serial console output
jerboa logs a3f8c2d1
# Hello from unikernel!

# Attach directly for real-time output (blocks until VM exits)
jerboa run hello:latest --attach
# Hello from unikernel!

# Run detached explicitly (default behavior)
jerboa run hello:latest -d

# Stop and clean up
jerboa stop a3f8c2d1
jerboa rm a3f8c2d1
```

{: .note }
`jerboa run` takes a built image name (`hello:latest`) or a path to a `.img` disk image file — **not** a raw ELF binary. Always run `jerboa build` first.

### 4. Run with ports and environment variables

```bash
# Expose port 8080 on your host → port 80 inside the VM
# Pass environment variables with -e
jerboa run myapp:latest -p 8080:80 -e PORT=80 -e APP_ENV=production --name web

# Check the port mapping in the VM details
jerboa inspect web
# {"id":"...","name":"web","ports":[{"host_port":8080,"guest_port":80,"protocol":"tcp"}],...}

# Auto-remove on exit
jerboa run hello:latest --rm
```

### 5. Managed networks and static IPs

Jerboa manages its own Linux bridge networks. Create one with `jerboa network create`, then attach VMs to it with `jerboa run --network` — Jerboa wires up a TAP interface per VM, attaches it to the bridge, and either auto-allocates an IP or uses the one you specify with `--ip`.

```bash
# Create a managed network — auto-allocates a /24 from 10.100.0.0/16
jerboa network create app

# Run a VM on it with an auto-assigned IP and a published port
jerboa run myapp:latest --network app -p 8080:80 --name web

# Or pin a static IP explicitly (still inside the network's subnet)
jerboa run myapp:latest --network app --ip 10.100.0.10 -p 8080:80 --name web2

# Inspect the network, list VMs and resolve them by name
jerboa network inspect app
jerboa dns resolve web --network app
jerboa dns list --network app
```

Publishing ports with `-p` requires a managed network (`--network`); there is no SLIRP fallback. Jerboa runs a userspace forwarder that listens on the host port and proxies connections to the VM over its bridge. See [Architecture → Networking]({% link architecture.md %}) for how this fits together, and [`jerboa network` / `jerboa dns`]({% link cli-reference.md %}#network-and-dns-commands) for the full command reference.

{: .note }
Managed networks, static IPs, and port publishing require Linux with `CAP_NET_ADMIN`/root (the relevant code is built only on Linux — see `internal/network/bridge_linux.go` and `tap.go`). `jerboa run -p` therefore requires `--network`; running without a network still works but exposes no ports. Under WSL2 on Windows, the daemon runs in the Linux VM and published ports are reachable from Windows via WSL2 localhost forwarding.

### 6. Use persistent volumes

```bash
# Create a named volume (1 GiB sparse disk image)
jerboa volume create mydata --size 1G

# Mount it into a VM
jerboa run myapp:latest -v mydata:/var/data --name app

# The volume persists after the VM stops
jerboa stop app
jerboa volume ls
# NAME    SIZE   CREATED
# mydata  1.0G   ...

# Remove a volume (irreversible)
jerboa volume rm mydata
```

---

## Running as a Service

### systemd (Linux)

Create `/etc/systemd/system/jerboad.service`:

```ini
[Unit]
Description=Jerboa Unikernel Daemon
After=network.target

[Service]
ExecStart=/usr/local/bin/jerboad --socket /var/run/jerboad.sock --qemu /usr/bin/qemu-system-x86_64
Restart=on-failure
User=root

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now jerboad
sudo systemctl status jerboad
```

---

## Keeping Up to Date

### Updating jerboa and jerboad

There is no self-update command. Check your installed version with `jerboa --version`, then replace the binaries manually with the latest release from
[github.com/AitorConS/jerboa/releases](https://github.com/AitorConS/jerboa/releases). The CLI (`jerboa`) and daemon (`jerboad`) are versioned together — update both to the same release.

#### Linux

The daemon runs natively as a `systemd` service. Download the new binaries, drop them into place, and restart the service:

```bash
BASE=https://github.com/AitorConS/jerboa/releases/latest/download

# Download the latest CLI and daemon
curl -fsSL "$BASE/jerboa-linux-amd64"  -o /tmp/jerboa
curl -fsSL "$BASE/jerboad-linux-amd64" -o /tmp/jerboad

# Stop the daemon, replace both binaries, start it again
sudo systemctl stop jerboad
sudo install -m 0755 /tmp/jerboad /usr/local/bin/jerboad
sudo install -m 0755 /tmp/jerboa  /usr/local/bin/jerboa
sudo systemctl start jerboad

# Verify
jerboa --version
jerboa status
```

Replace `latest` with a tag (e.g. `download/v0.1.1`) to pin a specific release. Re-running [`scripts/install.sh`](#linux-one-shot-install-recommended) achieves the same result.

#### Windows

The CLI is `jerboa.exe` on the host; the daemon (`jerboad`, a Linux binary) runs inside the dedicated `jerboa` WSL2 distro at `/usr/local/bin/jerboad`. Update both:

```powershell
# 1. Replace the host CLI. Windows cannot overwrite a running .exe, so close
#    any running jerboa first, then download jerboa-windows-amd64.exe from
#    the releases page and replace your existing jerboa.exe with it.
#    (e.g. save it over C:\Users\<you>\bin\jerboa.exe)

# 2. Download the Linux daemon binary next to it
curl.exe -fsSL https://github.com/AitorConS/jerboa/releases/latest/download/jerboad-linux-amd64 -o $env:TEMP\jerboad

# 3. Stop the daemon, copy the new binary into the distro, restart
jerboa daemon stop
wsl -d jerboa -u root -- cp "$(wslpath "$env:TEMP\jerboad")" /usr/local/bin/jerboad
wsl -d jerboa -u root -- chmod 0755 /usr/local/bin/jerboad
jerboa daemon start

# 4. Verify
jerboa --version
jerboa status
```

Only the daemon binary is swapped, so images, volumes, and other distro data are preserved. (`jerboa daemon install --force` re-imports the whole rootfs and is not needed for a routine update.)

### Updating the kernel tools

The kernel tools (`kernel.img`, `boot.img`, `mkfs`) are cached in `~/.jerboa/tools/` independently of the CLI.

```bash
# Check current and latest kernel version
jerboa kernel check

# Install the latest kernel
jerboa kernel update

# List all available kernel versions
jerboa kernel list

# Switch to a specific version
jerboa kernel use v0.1.0
```

---

## Next Steps

- [CLI Reference]({% link cli-reference.md %}) — all commands in detail
- [Compose]({% link compose.md %}) — run multi-service stacks
- [Architecture]({% link architecture.md %}) — how it works internally
