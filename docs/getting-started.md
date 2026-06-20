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
| Go | 1.25+ | Required only when building `uni`/`unid` from source |

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

Download the latest release from [GitHub Releases](https://github.com/AitorConS/UniCli/releases/tag/latest):

| Platform | Binary |
|---|---|
| Linux amd64 | `uni-linux-amd64`, `unid-linux-amd64` |
| Linux arm64 | `uni-linux-arm64`, `unid-linux-arm64` |
| Windows amd64 | `uni-windows-amd64.exe`, `unid-windows-amd64.exe` |

```bash
# Linux — download and install
curl -Lo /usr/local/bin/uni   https://github.com/AitorConS/UniCli/releases/latest/download/uni-linux-amd64
curl -Lo /usr/local/bin/unid  https://github.com/AitorConS/UniCli/releases/latest/download/unid-linux-amd64
chmod +x /usr/local/bin/uni /usr/local/bin/unid
```

### Build from source

```bash
git clone https://github.com/AitorConS/UniCli.git
cd UniCli
make build
# Produces: dist/uni  dist/unid
```

---

## Quick Start

### 1. Start the daemon

The daemon (`unid`) must run in the background before you can use the `uni` CLI.

```bash
# Linux (listens on /var/run/unid.sock)
sudo unid --qemu qemu-system-x86_64

# Without sudo (custom socket path)
unid --socket /tmp/unid.sock --qemu qemu-system-x86_64

# With observability enabled — Prometheus metrics + web dashboard
unid --socket /tmp/unid.sock --metrics-addr :9090 --ui-addr :8080
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
uni build ./hello --name hello
# sha256:abc123...  hello:latest
```

```powershell
# Windows
.\uni-windows-amd64.exe build hello-linux --name hello
# sha256:abc123...  hello:latest
```

On first run, `uni build` automatically downloads `mkfs`, `kernel.img`, and `boot.img` from the latest release into `~/.uni/tools/` (or `%USERPROFILE%\.uni\tools\` on Windows). On Windows, the build step runs through WSL2.

If a newer kernel version is available, `uni build` will prompt before building:

```
⚠  New kernel version available: v0.1.1 (installed: v0.1.0)
Update kernel before building? [y/N]
```

You can also manage the kernel tools explicitly — see [Kernel Commands]({% link cli-reference.md %}#kernel-commands) in the CLI reference.

### 2b. Or build directly from source

For Go, Node.js, Python, and Rust projects, `uni build` can skip the manual cross-compilation step entirely: point it at a source directory and it detects the language from project markers (`go.mod`, `package.json`, `pyproject.toml`/`requirements.txt`, `Cargo.toml`), compiles it, resolves a matching language runtime, and packages everything into one image.

**Example: a Node.js app**

```js
// hi.js
console.log("Hello from uni+node!");
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
uni build ./myapp --name myapp --pkg-source ops
uni run myapp:latest --attach
# Hello from uni+node!
```

Without `engines.node`, the driver defaults to Node 20. The same pattern applies to the other drivers:

| Language | Detected by | What happens |
|---|---|---|
| `go` | `go.mod` | Compiled with `CGO_ENABLED=0` into a static ELF binary — no runtime package needed |
| `node` | `package.json` | `npm install --production`, then bundled with a `node` runtime (version from `engines.node`, default `20`) |
| `python` | `pyproject.toml` / `requirements.txt` | Dependencies installed with `pip`, bundled with a `python` runtime (version from `requires-python`) |
| `rust` | `Cargo.toml` | Compiled with `cargo build --release` against a musl target into a static binary |

You can force a specific driver with `--lang go|node|python|rust`, cross-compile with `--platform linux/arm64`, and configure builds declaratively (including multi-stage builds) with a `unikernel.toml` file. See [`uni build`]({% link cli-reference.md %}#uni-build) for the complete flag reference and `unikernel.toml` format.

{: .important }
Building an HTTP server (Flask, Express, Next.js, ...)? Pass `--port <port>` to `uni build` so Nanos brings up its network stack at boot — without it the unikernel has no network and the server can't bind. See [Networking & Environment in the Image Manifest]({% link cli-reference.md %}#networking-environment-in-the-image-manifest).

**Framework examples:** the repo's `examples/` directory includes a minimal Flask app (`examples/flaskapp`, no `unikernel.toml` needed) and a Next.js app using `output: "standalone"` (`examples/nextapp`, using `[build] run` and `.unignore` — see [Framework Build Steps]({% link cli-reference.md %}#framework-build-steps)):

```bash
# Flask
uni build examples/flaskapp --pkg-source ops --pkg eyberg/python:3.10.6 --port 8080 --name flaskapp
uni run flaskapp -p 8080:8080

# Next.js
uni build examples/nextapp --pkg-source ops --port 3000 --name nextapp
uni run nextapp -p 3000:3000
```

### 3. Run it

```bash
uni run hello:latest
# a3f8c2d1-...

# Check it's running
uni ps
# ID                                    NAME  STATE    HEALTH   IMAGE
# a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f  -     running  unknown  hello:latest

# Read the serial console output
uni logs a3f8c2d1
# Hello from unikernel!

# Attach directly for real-time output (blocks until VM exits)
uni run hello:latest --attach
# Hello from unikernel!

# Run detached explicitly (default behavior)
uni run hello:latest -d

# Stop and clean up
uni stop a3f8c2d1
uni rm a3f8c2d1
```

{: .note }
`uni run` takes a built image name (`hello:latest`) or a path to a `.img` disk image file — **not** a raw ELF binary. Always run `uni build` first.

### 4. Run with ports and environment variables

```bash
# Expose port 8080 on your host → port 80 inside the VM
# Pass environment variables with -e
uni run myapp:latest -p 8080:80 -e PORT=80 -e APP_ENV=production --name web

# Check the port mapping in the VM details
uni inspect web
# {"id":"...","name":"web","ports":[{"host_port":8080,"guest_port":80,"protocol":"tcp"}],...}

# Auto-remove on exit
uni run hello:latest --rm
```

### 5. Managed networks and static IPs

Uni manages its own Linux bridge networks. Create one with `uni network create`, then attach VMs to it with `uni run --network` — Uni wires up a TAP interface per VM, attaches it to the bridge, and either auto-allocates an IP or uses the one you specify with `--ip`.

```bash
# Create a managed network — auto-allocates a /24 from 10.100.0.0/16
uni network create app

# Run a VM on it with an auto-assigned IP and a published port
uni run myapp:latest --network app -p 8080:80 --name web

# Or pin a static IP explicitly (still inside the network's subnet)
uni run myapp:latest --network app --ip 10.100.0.10 -p 8080:80 --name web2

# Inspect the network, list VMs and resolve them by name
uni network inspect app
uni dns resolve web --network app
uni dns list --network app
```

When you publish ports with `-p` on a managed network, Uni configures the iptables DNAT rules automatically so traffic reaches the VM through its bridge interface. See [Architecture → Networking]({% link architecture.md %}) for how this fits together, and [`uni network` / `uni dns`]({% link cli-reference.md %}#network-and-dns-commands) for the full command reference.

{: .note }
Managed networks, static IPs, and DNAT port forwarding require Linux with `CAP_NET_ADMIN`/root and `iptables` (the relevant code is built only on Linux — see `internal/network/bridge_linux.go` and `tap.go`). On Windows and macOS, `uni run -p` still works through QEMU's user-mode SLIRP networking, but `--network` and `--ip` are not available.

### 6. Use persistent volumes

```bash
# Create a named volume (1 GiB sparse disk image)
uni volume create mydata --size 1G

# Mount it into a VM
uni run myapp:latest -v mydata:/var/data --name app

# The volume persists after the VM stops
uni stop app
uni volume ls
# NAME    SIZE   CREATED
# mydata  1.0G   ...

# Remove a volume (irreversible)
uni volume rm mydata
```

---

## Running as a Service

### systemd (Linux)

Create `/etc/systemd/system/unid.service`:

```ini
[Unit]
Description=Uni Unikernel Daemon
After=network.target

[Service]
ExecStart=/usr/local/bin/unid --socket /var/run/unid.sock --qemu /usr/bin/qemu-system-x86_64
Restart=on-failure
User=root

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now unid
sudo systemctl status unid
```

---

## Keeping Up to Date

### Updating the CLI (`uni` and `unid`)

```bash
# Check if a newer version exists
uni upgrade check
# Installed CLI:  v0.1.0
# Running daemon: v0.1.0
# Latest:         v0.1.1
# Update available. Run `uni upgrade` to install v0.1.1.

# Install latest (prompts for confirmation)
uni upgrade

# Skip the prompt
uni upgrade --yes
```

`uni upgrade` replaces the running `uni` binary in-place and also updates `unid` if it is found in the same directory. On Windows the running binary is renamed to `.bak` before the new one is installed. After the upgrade completes successfully, old `.bak` files are cleaned up automatically.

### Updating the kernel tools

The kernel tools (`kernel.img`, `boot.img`, `mkfs`) are cached in `~/.uni/tools/` independently of the CLI.

```bash
# Check current and latest kernel version
uni kernel check

# Install the latest kernel
uni kernel update

# List all available kernel versions
uni kernel list

# Switch to a specific version
uni kernel use v0.1.0
```

---

## Next Steps

- [CLI Reference]({% link cli-reference.md %}) — all commands in detail
- [Compose]({% link compose.md %}) — run multi-service stacks
- [Architecture]({% link architecture.md %}) — how it works internally
