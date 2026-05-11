---
layout: default
title: CLI Reference
nav_order: 3
---

# CLI Reference
{: .no_toc }

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

## Global Flags

Every `uni` command accepts these flags:

| Flag | Default | Description |
|---|---|---|
| `--socket` | `/var/run/unid.sock` (Linux) / `%TEMP%\unid.sock` (Windows) | Path to the `unid` Unix socket |
| `--store` | `~/.uni/images` | Local image store directory |
| `--output` | `table` | Output format: `table` or `json` |

---

## VM Commands

### `uni run`

Create and immediately start a unikernel VM.

```
uni run <image> [flags]
```

`<image>` can be:
- A **file path**: `./myapp.img` — path to a pre-built bootable disk image
- A **name:tag reference**: `hello:latest` — looked up in the local image store

> **Note:** `uni run` requires a bootable disk image, not a raw ELF binary.
> To package a binary into an image first run `uni build --name <name> <binary>`,
> then `uni run <name>:latest`.

**Flags:**

| Flag | Default | Description |
|---|---|---|
| `--memory` | `256M` | VM memory (e.g. `256M`, `1G`, `4G`) |
| `--cpus` | `1` | Number of virtual CPUs |
| `-p`, `--port` | — | Publish port(s): `host:guest[/tcp\|udp]` (repeatable) |
| `-e`, `--env` | — | Set environment variable `KEY=VALUE` (repeatable) |
| `--env-file` | — | Read environment variables from file (one `KEY=VALUE` per line) |
| `--name` | — | Assign a human-readable name to the VM instance |
| `--rm` | `false` | Automatically remove the VM when it stops |
| `-v`, `--volume` | — | Mount a named volume: `name:guestpath[:ro]` (repeatable) |
| `--attach` | `false` | Attach to VM serial console (blocks until VM stops) |
| `-d`, `--detach` | `true` | Run VM in the background (overridden by `--attach`) |
| `--ip` | — | Static IP address, configured in the guest via fw_cfg (requires `--network`) |
| `--network` | — | Managed network name created by `uni network create` |
| `--health-check` | — | Health check probe: `tcp:PORT` or `http:PORT:/path` |
| `--restart` | — | Restart policy: `never`, `on-failure`, or `always[:max-retries]` |

**Examples:**

```bash
# Run from a pre-built disk image file
uni run ./myapp.img --memory 512M --cpus 2

# Run a built image by name
uni run myapp:latest

# Expose port 8080 on the host → port 80 inside the VM
uni run nginx:latest -p 8080:80

# Multiple ports and UDP
uni run myapp:latest -p 8080:80 -p 5353:53/udp

# Pass environment variables
uni run myapp:latest -e NODE_ENV=production -e PORT=3000

# Load env vars from a file
uni run myapp:latest --env-file .env

# Mount a named volume (create first with 'uni volume create')
uni run myapp:latest -v data:/var/data

# Read-only volume mount
uni run myapp:latest -v config:/etc/app:ro

# Named instance, auto-remove on exit
uni run hello:latest --name web --rm

# Attach to serial console (blocks until VM exits)
uni run hello:latest --attach

# Attach with a named instance and port
uni run myapp:latest --name api --attach -p 8080:8080

# Run on a managed network with auto-IP allocation
uni network create app
uni run myapp:latest --network app -p 8080:80

# Run on a managed network with explicit static IP
uni run myapp:latest --network app --ip 10.100.0.10 -p 8080:80

# Run with a health check (TCP probe on port 8080)
uni run myapp:latest --health-check tcp:8080

# Run with an HTTP health check
uni run myapp:latest --health-check http:8080:/healthz

# Restart automatically on failure (up to 5 times)
uni run myapp:latest --restart on-failure:5

# Always restart (unlimited retries)
uni run myapp:latest --restart always

# Output the VM ID for scripting
ID=$(uni run hello:latest --name api)
echo "Started VM: $ID"
```

**Output:**
```
a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f
```

With `--attach`, the command blocks and streams the VM's serial console output to stdout until the VM stops. No VM ID is printed in attach mode.

---

### `uni ps`

List all registered VMs with health status.

```
uni ps
```

**Examples:**

```bash
uni ps
# ID                                    NAME     STATE    HEALTH     IMAGE
# a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f  web      running  healthy    hello:latest
# b4e9d3e2-8c5f-5b2g-9d3e-2b3c4d5e6f7a  -        stopped  unknown    api:v2

# JSON output
uni --output json ps
```

**JSON output:**
```json
[
  {
    "id": "a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f",
    "state": "running",
    "name": "web",
    "health": "healthy",
    "image": "hello:latest"
  }
]
```

---

### `uni status`

Show a summary of the daemon and all VMs, including health and restart information.

```
uni status
```

**Examples:**

```bash
uni status
# Total:      3
# Running:    2
# Stopped:    1
# Healthy:    1
# Unhealthy:  0
#
# ID                                    NAME     STATE    HEALTH     RESTARTS  IMAGE
# a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f  web      running  healthy    0         hello:latest
# b4e9d3e2-8c5f-5b2g-9d3e-2b3c4d5e6f7a  api      running  starting   1         api:v2
# c5f0a4b3-9d6e-4c2f-a1b3-5d6e7f8a9b0c  -        stopped  -          0         worker:latest

# JSON output
uni --output json status
```

---

### `uni logs`

Print captured serial console output (stdout + stderr) for a VM.

```
uni logs <id>
```

**Example:**

```bash
uni logs a3f8c2d1
# Hello from unikernel!
# tick 1
# tick 2
```

{: .note }
Logs are buffered in memory by `unid`. They are lost when the daemon restarts.
For real-time streaming, use `uni run --attach` instead, which blocks and
streams the serial console output directly to your terminal as the VM runs.

---

### `uni inspect`

Display full details for a VM as JSON.

```
uni inspect <id>
```

**Example:**

```bash
uni inspect a3f8c2d1
```

```json
{
  "id": "a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f",
  "state": "running",
  "image": "hello:latest",
  "name": "web",
  "memory": "256M",
  "cpus": 1,
  "ports": [
    {"host_port": 8080, "guest_port": 80, "protocol": "tcp"}
  ],
  "env": ["NODE_ENV=production", "PORT=3000"],
  "volumes": [
    {"disk_path": "/home/user/.uni/volumes/data/disk.img", "guest_path": "/var/data", "read_only": false}
  ],
  "created_at": "2026-04-19T10:00:00Z",
  "started_at": "2026-04-19T10:00:01Z"
}
```

---

### `uni stop`

Gracefully stop a running VM.

```
uni stop <id> [--force]
```

**Shutdown sequence (without `--force`):**
1. Send `SIGTERM` to the QEMU process
2. Wait up to **30 seconds** for the VM to exit cleanly
3. Send `SIGKILL` if still running after the grace period

| Flag | Default | Description |
|---|---|---|
| `--force` | `false` | Skip grace period, send `SIGKILL` immediately |

**Examples:**

```bash
# Graceful shutdown
uni stop a3f8c2d1

# Immediate kill
uni stop --force a3f8c2d1
```

---

### `uni rm`

Remove a stopped VM from the registry.

```
uni rm <id>
```

{: .warning }
The VM must be in `stopped` state. Run `uni stop <id>` first.

**Example:**

```bash
uni stop a3f8c2d1
uni rm a3f8c2d1
```

---

### `uni cp`

Copy files to or from a stopped VM disk image. The `dump` and `mkfs` tools are downloaded automatically on first use from the kernel release.

```
uni cp <src> <dst>
```

One operand must be a VM reference in the form `id:path`, the other a local file path. Copying **to** a VM rebuilds the disk image with the new file included.

**Examples:**

```bash
# Copy a file from a stopped VM to the host
uni cp myvm:/etc/config.json ./config.json
# copied myvm:/etc/config.json → ./config.json

# Copy a file from the host into a stopped VM
uni cp ./local.conf myvm:/etc/config.json
# copied ./local.conf → myvm:/etc/config.json

# Copy from a VM identified by prefix
uni cp a3f8:/var/log/app.log ./app.log
```

---

### `uni exec`

Send a signal to a running VM process.

```
uni exec <id> --signal <SIG>
```

| Flag | Default | Description |
|---|---|---|
| `--signal` | `SIGTERM` | Signal name (e.g. `SIGTERM`, `SIGHUP`) or number (e.g. `15`) |

**Examples:**

```bash
# Reload configuration (if the app handles SIGHUP)
uni exec a3f8c2d1 --signal SIGHUP

# Send signal by number
uni exec a3f8c2d1 --signal 1
```

**Supported signal names:** `SIGTERM`, `SIGINT`, `SIGKILL`, `SIGHUP`, `SIGQUIT`, `SIGUSR1`, `SIGUSR2`

---

## Image Commands

### `uni build`

Build a unikernel image from a static ELF binary.

```
uni build <binary> [flags]
```

The binary must be a **static Linux ELF** (`GOOS=linux`, no dynamic library dependencies). Go binaries built with `CGO_ENABLED=0 GOOS=linux` are ideal.

| Flag | Default | Description |
|---|---|---|
| `--name` | Binary filename | Image name |
| `--tag` | `latest` | Image tag |
| `--memory` | `256M` | Default VM memory baked into the image |
| `--cpus` | `1` | Default CPU count baked into the image |
| `--mkfs` | *(auto-downloaded to `~/.uni/tools/mkfs`)* | Path to Nanos mkfs binary — overrides auto-download (env: `UNI_MKFS`) |
| `-U`, `--update-kernel` | `false` | Auto-approve kernel update if one is available (skips the `[y/N]` prompt) |
| `--pkg` | — | Include package in the image (repeatable). Downloads, extracts, and includes the package files |

If the kernel tools are already cached and a newer kernel version is available, `uni build` will prompt before proceeding:

```
⚠  New kernel version available: v0.1.1 (installed: v0.1.0)
Update kernel before building? [y/N]
```

**Examples:**

```bash
# Basic build
uni build ./hello

# Custom name and tag
uni build ./myapi --name api --tag v1.2.0

# With resource defaults
uni build ./api --name api --tag latest --memory 512M --cpus 2

# Include a runtime package
uni build ./myapp --name myapp --pkg node:20

# Include multiple packages
uni build ./myapp --name myapp --pkg node:20 --pkg redis:7
```

**Output:**
```
sha256:abc123def456...  api:latest
```

---

### `uni images`

List all images in the local store.

```
uni images
```

**Example:**

```bash
uni images
# DIGEST              NAME   TAG     CREATED               SIZE
# sha256:abc123def4   hello  latest  2026-04-19T10:00:00Z  12.0MB
# sha256:def456ghi7   api    v1.2.0  2026-04-19T11:00:00Z  24.3MB
```

---

### `uni rmi`

Remove an image from the local store.

```
uni rmi <ref>
```

`<ref>` can be `name:tag` or `sha256:<hex>`.

**Example:**

```bash
uni rmi hello:latest
# hello:latest
```

---

### `uni push`

Push a local image to a registry.

`uni push` now prefers the OCI Distribution flow (`/v2/<name>/blobs/...` + `/v2/<name>/manifests/<tag>`).
If the target registry does not support OCI endpoints yet, it automatically falls back to the legacy
`/v2/images` API.

Registry auth/TLS options are available as global flags:

- `--registry-token` (or `UNI_REGISTRY_TOKEN`) to send bearer/JWT auth
- `--registry-ca-cert` (or `UNI_REGISTRY_CA_CERT`) to trust a custom CA
- `--registry-insecure` (or `UNI_REGISTRY_INSECURE=true`) to skip TLS verification in development

```
uni push <ref> <registry-url>
```

**Example:**

```bash
uni push hello:latest http://registry.example.com:5000
# pushed hello:latest to http://registry.example.com:5000
```

---

### `uni pull`

Pull an image from a registry into the local store.

`uni pull` now prefers the OCI Distribution flow and automatically falls back to the legacy
`/v2/images` API when needed.

The same global registry auth/TLS flags used by `uni push` apply to `uni pull`.

```
uni pull <ref> <registry-url>
```

**Example:**

```bash
uni pull hello:latest http://registry.example.com:5000
# sha256:abc123...  hello:latest
```

---

## Package Commands

Manage pre-packaged files that can be included in images at build time. Packages are cached locally in `~/.uni/packages/`.

### `uni pkg list`

List locally cached packages.

```
uni pkg list
```

```bash
uni pkg list
# NAME       VERSION   SIZE
# redis      7.2       12.5MB
# nginx      1.25      8.3MB
```

---

### `uni pkg search`

Search the remote package index.

```
uni pkg search <query>
```

```bash
uni pkg search redis
# NAME       VERSION   SIZE      DESCRIPTION
# redis      7.2       12.5MB    In-memory data store
# redis-cli  7.2       3.1MB     Redis command-line client
```

---

### `uni pkg get`

Download and install a package from the remote index.

```
uni pkg get <name>[:version]
```

```bash
# Install the latest version
uni pkg get redis

# Install a specific version
uni pkg get redis:7.2
```

---

### `uni pkg remove`

Remove locally cached package(s). Without a version suffix, all versions of the package are removed.

```
uni pkg remove <name>[:version]
```

```bash
# Remove a specific version
uni pkg remove redis:7.2

# Remove all locally cached versions
uni pkg remove redis
# Removed all versions of package redis.
```

---

## Volume Commands

Volumes are named persistent disk images that survive VM restarts. Create a volume once, then mount it into any VM with `-v`.

### `uni volume create`

Create a new named volume.

```
uni volume create <name> [--size <size>]
```

| Flag | Default | Description |
|---|---|---|
| `--size` | `1G` | Volume size: `512M`, `1G`, `2G`, etc. |

**Example:**

```bash
uni volume create mydata --size 2G
# mydata

uni volume create config --size 512M
# config
```

---

### `uni volume ls`

List all volumes.

```
uni volume ls [--output json]
```

```bash
uni volume ls
# NAME     SIZE   CREATED
# mydata   2.0G   2026-04-25 18:00:00
# config   512.0M 2026-04-25 18:01:00
```

---

### `uni volume inspect`

Show full details for a volume as JSON.

```
uni volume inspect <name>
```

```bash
uni volume inspect mydata
```

```json
{
  "id": "mydata",
  "disk_path": "/home/user/.uni/volumes/mydata/disk.img",
  "size_bytes": 2147483648,
  "created_at": "2026-04-25T18:00:00Z"
}
```

---

### `uni volume rm`

Remove a volume and its disk image permanently.

```
uni volume rm <name>
```

{: .warning }
This is irreversible. All data stored in the volume will be lost.

```bash
uni volume rm mydata
# mydata
```

---

## Network and DNS Commands

### `uni network create`

Create a managed network. When `--subnet` is omitted, Uni auto-allocates a `/24` from `10.100.0.0/16`.

```
uni network create <name> [--subnet <cidr>] [--driver bridge]
```

### `uni dns resolve`

Resolve a running VM/service name to its IP address.

```
uni dns resolve <name> [--network <name>]
```

Examples:

```bash
uni dns resolve frontend --network app
uni dns resolve frontend.app
```

If the same service name exists in multiple networks, `--network` (or `name.network`) is required.

### `uni dns list`

List resolvable records from running VMs.

```
uni dns list [--network <name>]
```

---

## Kernel Commands

Manage the kernel tools (`kernel.img`, `boot.img`, `mkfs`) cached in `~/.uni/tools/`. The kernel is versioned independently from the CLI.

### `uni kernel check`

Show the installed kernel version and whether a newer one is available.

```
uni kernel check
```

```bash
uni kernel check
# Installed kernel: v0.1.0
# Latest kernel:    v0.1.1
# Update available. Run `uni kernel update` to install v0.1.1.
```

---

### `uni kernel update`

Download and install the latest kernel tools.

```
uni kernel update [--yes]
```

| Flag | Default | Description |
|---|---|---|
| `-y`, `--yes` | `false` | Skip confirmation prompt |

```bash
uni kernel update
# New kernel version available: v0.1.1 (installed: v0.1.0)
# Update? [y/N] y
# Downloading kernel.img...
# Kernel updated to v0.1.1.
```

---

### `uni kernel list`

List all available kernel versions, newest first. The currently installed version is marked with `*`.

```
uni kernel list
```

```bash
uni kernel list
# * v0.1.1
#   v0.1.0
```

---

### `uni kernel use`

Switch to a specific kernel version.

```
uni kernel use <version> [--yes]
```

| Flag | Default | Description |
|---|---|---|
| `-y`, `--yes` | `false` | Skip confirmation prompt |

```bash
uni kernel use v0.1.0
# Switching kernel: v0.1.1 → v0.1.0
# Proceed? [y/N] y
# Kernel switched to v0.1.0.
```

---

## Upgrade Commands

Manage the `uni` and `unid` binaries themselves. The CLI is versioned independently from the kernel.

### `uni upgrade`

Download and install the latest `uni` (and `unid` if found alongside it), replacing the running binaries in-place.

```
uni upgrade [--yes]
```

| Flag | Default | Description |
|---|---|---|
| `-y`, `--yes` | `false` | Skip confirmation prompt |

```bash
uni upgrade
# Installed: v0.1.0
# Latest:    v0.1.1
# New version available: v0.1.1
# Upgrade? [y/N] y
# Downloading uni-linux-amd64...
# Downloading unid-linux-amd64...
# Upgraded to v0.1.1.
```

{: .note }
On Windows, the running binary is renamed to `.bak` before the new one is placed in its position, since Windows does not allow overwriting a running executable directly. After the upgrade completes successfully, old `.bak` files are cleaned up automatically.

---

### `uni upgrade check`

Show the installed CLI version and whether a newer one is available, without installing anything.

```
uni upgrade check
```

```bash
uni upgrade check
# Installed: v0.1.0
# Latest:    v0.1.1
# Update available. Run `uni upgrade` to install v0.1.1.
```

---

### `uni upgrade list`

List all available CLI versions, newest first. The currently running version is marked with `*`.

```
uni upgrade list
```

```bash
uni upgrade list
#   v0.1.1
# * v0.1.0
```

---

## Compose Commands

See the full [Compose Reference]({% link compose.md %}) for the file format.

### `uni compose up`

Start all services defined in a compose file, in dependency order.

```
uni compose up <compose-file>
```

**Example:**

```bash
uni compose up stack.yaml
# started backend → a3f8c2d1-...
# started frontend → b4e9d3e2-...
```

---

### `uni compose down`

Stop all services from a compose file, in reverse dependency order.

```
uni compose down <compose-file> [--force] [--volumes]
```

| Flag | Default | Description |
|---|---|---|
| `--force` | `false` | SIGKILL immediately |
| `--volumes` | `false` | Remove volumes defined in the compose `volumes:` section |

---

### `uni compose ps`

List the state of all services in a compose stack.

```
uni compose ps <compose-file>
```

```bash
uni compose ps stack.yaml
# SERVICE   ID                                    STATE
# backend   a3f8c2d1-7b4e-4a1f-8c2d-...          running
# frontend  b4e9d3e2-8c5f-5b2g-9d3e-...          running

uni --output json compose ps stack.yaml
```

---

### `uni compose logs`

Print serial console output for a specific service.

```
uni compose logs <compose-file> <service>
```

```bash
uni compose logs stack.yaml backend
```

---

## VM States

```
created → starting → running → stopping → stopped
```

| State | Description |
|---|---|
| `created` | VM registered, QEMU not started yet |
| `starting` | QEMU process being launched |
| `running` | QEMU process alive and booting/running |
| `stopping` | Stop signal sent, waiting for process exit |
| `stopped` | QEMU process has exited |

{: .note }
A VM in `stopped` state can be removed with `uni rm`. It cannot be restarted — create a new VM with `uni run`.

When using `uni run --attach`, the command blocks until the VM reaches the `stopped` state, streaming all serial console output to stdout during the `running` state.
