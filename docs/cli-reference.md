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
| `--verify` | `off` | Image signature verification: `off`, `warn`, `enforce` |
| `--cpu-shares` | `0` | cgroup v2 CPU weight (1–10000, 0=no limit, Linux only) |
| `--memory-max` | — | cgroup v2 memory hard limit (e.g. `512M`, `1G`; Linux only) |
| `--disk-iops` | `0` | Disk I/O throttle: max IOPS for boot disk (0=no limit) |
| `--disk-bps` | — | Disk I/O throttle: max bytes/sec for boot disk (e.g. `10M`; 0=no limit) |

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

### `uni stats`

Show resource usage for a single VM: CPU percentage, memory, and network I/O.

```
uni stats <id> [flags]
```

**Flags:**

| Flag | Default | Description |
|---|---|---|
| `-w`, `--watch` | `false` | Continuously watch stats (refresh every interval) |
| `-i`, `--interval` | `2s` | Watch interval (e.g. `1s`, `5s`) |

**Examples:**

```bash
# Single snapshot
uni stats a3f8c2d1
# ID        a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f
# State     running
# CPU       12.5%
# Memory    256.0 MiB
# Net RX    1.5 KiB
# Net TX    3.2 KiB
# Source    procfs

# Continuous watch (refresh every 2 seconds)
uni stats a3f8c2d1 --watch

# Custom watch interval
uni stats a3f8c2d1 --watch --interval 5s

# JSON output
uni stats a3f8c2d1 --output json
```

{: .note }
On non-Linux platforms, CPU and memory stats fall back to a `fallback` source with zero values. Full `procfs`-based stats are available only on Linux where the QEMU process `/proc` entries are accessible.

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
  "started_at": "2026-04-19T10:00:01Z",
  "health": "healthy",
  "restart_policy": "on-failure",
  "restart_count": 0,
  "disk_iops": 1000,
  "disk_bps": 10485760
}
```

Fields only present when non-zero:

| Field | Description |
|---|---|
| `disk_iops` | Max I/O operations per second for the boot disk (set via `--disk-iops`) |
| `disk_bps` | Max bytes per second for the boot disk in bytes (set via `--disk-bps`) |
| `restart_policy` | Active restart policy (`never`, `on-failure`, `always`) |
| `restart_count` | Number of times this VM has been automatically restarted |
| `daemon_recovered` | `true` when the VM was recovered after a daemon restart |

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

### `uni exec`

Request graceful shutdown or immediate termination of a running VM.

```
uni exec <id> --signal <SIG>
```

| Flag | Default | Description |
|---|---|---|
| `--signal` | `SIGTERM` | Signal name (e.g. `SIGTERM`, `SIGKILL`) or number (e.g. `9`) |

Signals are delivered via QEMU Machine Protocol (QMP) over TCP, which works on Linux, macOS, and Windows without admin privileges.

| Signal | Effect |
|---|---|
| `SIGTERM` (and others) | Sends an ACPI power-button event to the guest OS for graceful shutdown |
| `SIGKILL` | Immediately terminates the QEMU host process |

**Examples:**

```bash
# Request graceful guest shutdown
uni exec a3f8c2d1 --signal SIGTERM

# Immediately kill the QEMU process
uni exec a3f8c2d1 --signal SIGKILL
```

**Supported signal names:** `SIGTERM`, `SIGINT`, `SIGKILL`, `SIGHUP`, `SIGQUIT`, `SIGUSR1`, `SIGUSR2`

---

## Image Commands

### `uni build`

Build a unikernel image from a static ELF binary or a source directory.

```
uni build <path> [flags]
```

`<path>` can be:
- A **file path**: `./hello` — path to a pre-compiled static ELF binary (legacy mode)
- A **directory**: `.` — build from source using a language driver (requires `--lang` or auto-detection)

When `<path>` is a directory, `uni build` detects the language from project markers (`go.mod`, `package.json`, etc.) or uses `--lang` explicitly, compiles the project, and packages the result into a unikernel image.

| Flag | Default | Description |
|---|---|---|
| `--name` | Binary/directory filename | Image name |
| `--tag` | `latest` | Image tag |
| `--memory` | `256M` | Default VM memory baked into the image |
| `--cpus` | `1` | Default CPU count baked into the image |
| `--mkfs` | *(auto-downloaded to `~/.uni/tools/mkfs`)* | Path to Nanos mkfs binary — overrides auto-download (env: `UNI_MKFS`) |
| `-U`, `--update-kernel` | `false` | Auto-approve kernel update if one is available (skips the `[y/N]` prompt) |
| `--pkg` | — | Include package in the image (repeatable), e.g. `node:20`. Downloads, extracts, and includes the package files |
| `--pkg-source` | `uni` | Source to resolve `--pkg` (and language-driver auto-detected runtime packages) from: `uni` or `ops` |
| `--lang` | *(auto-detect)* | Build from source directory with language driver (`go`, `node`, `python`, `rust`, `raw`) |
| `--platform` | *(native)* | Target platform for cross-compilation (e.g. `linux/amd64`, `linux/arm64`) |
| `--port` | `0` | Declared service port; enables the `network` section in the image manifest (required for any HTTP server to bind — see [Networking & Environment in the Image Manifest](#networking-environment-in-the-image-manifest)) |

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

# Build a Node.js project from source — the language driver detects
# package.json, reads "engines.node" for the version, and resolves the
# matching runtime package automatically (no --pkg needed)
uni build ./myapp --name myapp --pkg-source ops

# Build from source directory (Go project)
uni build --lang go .

# Build from source directory (auto-detect language)
uni build .

# Cross-compile for ARM64
uni build --lang go --platform linux/arm64 .

# Build with unikernel.toml config
uni build .

# Build a web app — declare its port so Nanos brings up the network stack
uni build ./myapi --name myapi --port 8080
```

**Language Drivers:**

| Language | Detect | Build | Notes |
|---|---|---|---|
| `go` | `go.mod` | `go build` with `CGO_ENABLED=0` | Static ELF binary |
| `node` | `package.json` | `npm install --production` | Uses `node` runtime package |
| `python` | `pyproject.toml` / `requirements.txt` | `pip install -r requirements.txt` | Uses `python` runtime package |
| `rust` | `Cargo.toml` | `cargo build --release --target <triple>` | Static ELF binary via musl |
| `raw` | *none — explicit `lang = "raw"` only* | `[build] run` only (no compilation) | Runtime binary and arguments come from `[program]` — see *Generic Runtimes* below |

**`unikernel.toml` Configuration:**

When `uni build` is run on a directory, it automatically reads `unikernel.toml` if present:

```toml
[build]
lang = "go"
entrypoint = "cmd/server"
args = ["-v"]
run = ["go generate ./..."]

[run]
memory = "512M"
cpus = 2
ports = ["8080:80", "9090:9090"]

[env]
NODE_ENV = "production"
```

| Section | Field | Description |
|---|---|---|
| `[build]` | `lang` | Force a specific language driver (`go`, `node`, `python`, `rust`, `raw`), skipping auto-detection |
| `[build]` | `entrypoint` | Override the driver's default entrypoint (e.g. a custom server script path) |
| `[build]` | `args` | Extra arguments passed to the build tool |
| `[build]` | `run` | Shell commands executed in the project directory **before** the language driver runs — see [Framework Build Steps](#framework-build-steps) below |
| `[run]` | `memory`, `cpus`, `ports` | Default VM resource and port settings baked into the image, used when `uni run` is called without overriding flags |
| `[env]` | *(any key)* | Environment variables baked into the image's Nanos manifest — see [Networking & Environment in the Image Manifest](#networking-environment-in-the-image-manifest) |
| `[program]` | `path` | Runtime binary to execute, resolved against `--pkg` files (exact guest path, path suffix, or basename). Required when `lang = "raw"`; invalid otherwise |
| `[program]` | `args` | Arguments passed to the resolved binary — Docker `CMD` equivalent |

**Framework Build Steps:**

`[build] run` is a list of shell commands executed in the project directory **before** the language driver packages the project — the equivalent of `RUN` instructions in a Dockerfile. Use it for any build step a framework requires: `npm run build`, `nuxt build`, `python manage.py collectstatic`, etc. Commands run via `sh -c` on Unix and `cmd /c` on Windows, with output streamed to stderr. If any command fails, the build aborts.

**Example: Next.js with `output: "standalone"`**

```toml
[build]
lang = "node"
run = ["npm install", "npm run build"]
entrypoint = ".next/standalone/server.js"
```

`.next` and `node_modules` are excluded from the build context by default (see [`.unignore` File](#unignore-file)); a project-level `.unignore` with `!.next` and `!node_modules` re-includes the standalone server output and its pruned dependencies. See `examples/nextapp/` for the complete working setup, and `examples/flaskapp/` for a Python project that needs no `unikernel.toml` at all (the Python driver's defaults are sufficient).

**Multi-stage Builds:**

`unikernel.toml` also supports multi-stage builds with `[[stages]]`. Each stage is built independently, and `copy_from` directives copy artifacts from previous stages:

```toml
[[stages]]
name = "builder"
lang = "go"
entrypoint = "cmd/server"

[[stages]]
name = "runtime"
lang = "node"
entrypoint = "server.js"

[[stages.copy_from]]
stage = "builder"
src = "/app/server"
dst = "server"
```

The final stage's output is used as the image binary. Earlier stages produce intermediate build artifacts that can be copied into later stages.

**Generic Runtimes (`lang = "raw"`):**

For runtimes without a dedicated driver (Java, .NET, Ruby, PHP, ...), `lang = "raw"` skips compilation entirely: `[build] run` performs the build, and `[program]` declares the runtime binary — resolved against the package files supplied via `--pkg` — and its arguments. This is the equivalent of Docker's `ENTRYPOINT` (`path`) plus `CMD` (`args`).

```toml
[build]
lang = "raw"
run = ["mvn -q -DskipTests package"]

[program]
path = "java"               # resolved against --pkg files: exact guest path,
                             # path suffix (e.g. "jdk-21/bin/java"), or basename
args = ["-jar", "/app.jar"] # passed through literally as argv[1..]
```

`target` is excluded from the build context by default (see [`.unignore` File](#unignore-file)); re-include the Maven output jar:

```
!target
!target/*.jar
```

Find a JDK package from the `ops` ecosystem and build:

```bash
uni pkg search openjdk --source ops
uni build ./myjavaapp --pkg-source ops --pkg eyberg/openjdk-21 --port 8080 --name myjavaapp
uni run myjavaapp -p 8080:8080
```

> **Note:** `lang = "raw"` is not currently supported inside `[[stages]]` (multi-stage builds).

**`.unignore` File:**

Exclude files from the build context with `.unignore` (similar to `.dockerignore`):

```
# Comment
*.log
build/
.git
node_modules
!important.log
```

The following patterns are **always excluded**, even without a `.unignore` file:

```
.git
.uni-build
node_modules
__pycache__
.tox
venv
.venv
dist
.next
target
```

Patterns are evaluated in order, gitignore-style: a later `!pattern` re-includes a path excluded by an earlier pattern — including the always-excluded defaults above. This is required for frameworks whose runtime output lives inside one of the default-excluded directories, e.g. a Next.js `output: "standalone"` build, where `.next/standalone/server.js` and its pruned `.next/standalone/node_modules` must ship in the image:

```
!.next
!node_modules
```

**Networking & Environment in the Image Manifest:**

Two pieces of information are baked into the image's Nanos manifest at build time, and both are required for HTTP servers built from source:

- **`--port`** — when set to a non-zero value, adds a `network` section to the manifest (`network:(ip:10.0.2.15 gateway:10.0.2.2 netmask:255.255.255.0)`) so Nanos initializes its network stack at boot. Without it, the unikernel has no network and any HTTP server fails to bind.
- **`[env]`** in `unikernel.toml` — each key/value pair is written into the manifest's `environment:(...)` section, sorted by key, and is available to the running process (e.g. `NODE_ENV = "production"`).

The Python driver also sets `PYTHONPATH=/packages` automatically whenever `requirements.txt` is present, since `pip install --target packages` installs dependencies outside Python's default `sys.path`. When `--pkg-source ops` is used, environment variables declared in the resolved `ops` package's manifest (e.g. `HOME` for `eyberg/python`) are merged in as well; driver-provided values (like `PYTHONPATH`) take precedence on key conflicts.

```bash
# Flask app — PYTHONPATH set automatically, --port enables the network stack
uni build examples/flaskapp --pkg-source ops --pkg eyberg/python:3.10.6 --port 8080 --name flaskapp
uni run flaskapp -p 8080:8080
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

### `uni sign`

Sign a local image with an Ed25519 key pair. If no key pair exists, one is generated automatically and stored in `~/.uni/keys/`.

```
uni sign <image>
```

**Example:**

```bash
uni sign hello:latest
# signed hello:latest (key a1b2c3d4e5f67890, digest sha256:...)
```

---

### `uni verify`

Verify the Ed25519 signature of a local image. Fails if the image has no signature or the signature is invalid.

```
uni verify <image>
```

**Example:**

```bash
uni verify hello:latest
# verified hello:latest (key a1b2c3d4e5f67890, digest sha256:...)
```

---

## Package Commands

Manage pre-packaged runtime files that can be included in images at build time (with `uni build --pkg`) or run directly (with `uni pkg load`).

Uni can fetch packages from two sources, selected with the `--source` flag on `list`, `search`, `get`, and `remove`:

| Source | Identifier format | Cached in | Description |
|---|---|---|---|
| `uni` (default) | `<name>[:<version>]` | `~/.uni/packages/` | Uni's own package index |
| `ops` | `<namespace>/<name>[:<version>]` | `~/.uni/packages-ops/` | The [nanovms/ops](https://ops.city) package ecosystem (`eyberg/node`, `eyberg/python`, …) |

> **Tip:** When building from a source directory with a language driver (`--lang node`, `--lang python`, …), `uni build` resolves the matching runtime package automatically — you don't normally need to run `uni pkg get` yourself. See [`uni build`](#uni-build) and [Getting Started]({% link getting-started.md %}) for the full workflow, including how to use `ops` packages such as `eyberg/node:v11.5.0`.

### `uni pkg list`

List locally cached packages.

```
uni pkg list [--source uni|ops] [--output-json]
```

| Flag | Default | Description |
|---|---|---|
| `--source` | `uni` | Package source: `uni` or `ops` |
| `--output-json` | `false` | Print as JSON instead of a table |

```bash
uni pkg list
# NAME       VERSION   RUNTIME   DESCRIPTION
# redis      7.2       redis     In-memory data store
# node       20        node      Node.js runtime

# List cached ops packages
uni pkg list --source ops
# NAMESPACE   NAME    VERSION   LANGUAGE   ARCH
# eyberg      node    v11.5.0   node       amd64
```

---

### `uni pkg search`

Search the remote package index.

```
uni pkg search <query> [--source uni|ops] [--output-json]
```

| Flag | Default | Description |
|---|---|---|
| `--source` | `uni` | Package source: `uni` or `ops` |
| `--output-json` | `false` | Print as JSON instead of a table |

```bash
uni pkg search redis
# NAME       VERSION   RUNTIME   DESCRIPTION
# redis      7.2       redis     In-memory data store
# redis-cli  7.2       redis     Redis command-line client

# Search the ops package hub
uni pkg search node --source ops
# NAMESPACE   NAME   VERSION   LANGUAGE   ARCH      DESCRIPTION
# eyberg      node   v11.5.0   node       amd64     Node.js v11.5.0
# eyberg      node   v16.5.0   node       amd64     Node.js v16.5.0
```

---

### `uni pkg get`

Download and install a package from the remote index.

```
uni pkg get <name>[:version] [--source uni|ops]
```

| Flag | Default | Description |
|---|---|---|
| `--source` | `uni` | Package source: `uni` or `ops` |

```bash
# Install the latest version from the uni index
uni pkg get redis

# Install a specific version
uni pkg get redis:7.2

# Install an ops package (note the <namespace>/<name>:<version> form)
uni pkg get eyberg/node:v11.5.0 --source ops
# Ops package eyberg/node v11.5.0 installed.
```

---

### `uni pkg remove`

Remove locally cached package(s). Without a version suffix, all versions of the package are removed. Errors if the package is not found locally.

```
uni pkg remove <name>[:<version>] [--source uni|ops]
uni pkg rm    <name>[:<version>] [--source uni|ops]   # alias
```

| Flag | Default | Description |
|---|---|---|
| `--source` | `uni` | Package source: `uni` or `ops` |

```bash
# Remove a specific version
uni pkg remove redis:7.2

# Remove all locally cached versions
uni pkg remove redis
# Removed all versions of package redis.

# Remove an ops package
uni pkg remove eyberg/node:v11.5.0 --source ops
```

---

### `uni pkg load`

Download a package, build a unikernel image from it, and (optionally) print the command to run it — a one-step shortcut comparable to `ops pkg load`.

```
uni pkg load <package> [--source uni|ops] [-d|--detach]
```

| Flag | Default | Description |
|---|---|---|
| `--source` | `uni` | Package source: `uni` or `ops` |
| `-d`, `--detach` | `false` | Build the image only; don't print run instructions |

```bash
# Build an image from a cached uni package
uni pkg load myruntime:1.0.0

# Download an ops package, build, and get run instructions
uni pkg load eyberg/node:v16.5.0 --source ops
# Built image pkg-load:latest (sha256:abc123...)
# Run with: uni run pkg-load:latest
```

{: .note }
The resulting image is always named `pkg-load`. For a properly named, reproducible image built from your own source code (with the package wired in automatically), prefer `uni build --lang <lang> <dir>` — see [`uni build`](#uni-build).

---

### `uni pkg create`

Create a local package from a binary and optional additional files. The package archive is stored in the local package cache.

```
uni pkg create <name>[:<version>] <binary> [--libs <file>...] [--description <desc>] [--runtime <runtime>] [--missing-files]
```

If no version is specified, `1.0.0` is used as the default.

The `--missing-files` flag analyses the binary with `ldd` and reports shared library dependencies that are missing from the local filesystem. This is useful for identifying which libraries need to be included with `--libs`.

```bash
# Create a package from a static binary
uni pkg create myapp:1.2.0 ./myapp --description "My application" --runtime custom

# Create a package with additional library files
uni pkg create myapp:1.2.0 ./myapp --libs ./libmyapp.so --description "With shared lib"

# Check for missing shared library dependencies
uni pkg create myapp:1.2.0 ./myapp --missing-files
# Missing shared libraries detected (not on local filesystem):
#   /lib/x86_64-linux-gnu/libssl.so.3
#   /lib/x86_64-linux-gnu/libcrypto.so.3
# Consider adding these with --libs or re-running with the binary on a Linux system.

# Create with auto-resolved shared libs from ldd
uni pkg create myapp:1.2.0 ./myapp

# Create with auto-default version (1.0.0)
uni pkg create myapp ./myapp
```

---

### `uni pkg from-docker`

Extract a binary and its shared library dependencies from a Docker image, creating a local package. Uses `docker create` + `docker cp` to extract the binary, then runs `ldd` inside the container to discover shared libraries.

```
uni pkg from-docker <name>[:<version>] <image> --file <path> [--libs <path>...] [--description <desc>] [--runtime <runtime>]
```

| Flag | Default | Description |
|---|---|---|
| `--file` | *(required)* | Path to the binary inside the Docker image |
| `--libs` | — | Additional library paths inside the container to include (repeatable) |
| `--description` | `""` | Package description |
| `--runtime` | `""` | Runtime family (e.g. `node`, `python`) |

```bash
# Extract Node.js from the official Docker image
uni pkg from-docker node:20 node:20 --file /usr/local/bin/node --runtime node

# Extract Redis with extra libraries
uni pkg from-docker redis:7 redis:7 --file /usr/local/bin/redis-server --libs /usr/local/bin/redis-cli --runtime redis

# Extract with auto-detected shared libraries
uni pkg from-docker myapp:1.0 myapp:latest --file /app/myapp --description "My app from Docker"
```

---

### `uni pkg push`

Push a locally cached package to a remote package index. The index server must support `POST /packages` with multipart form data (archive + metadata).

```
uni pkg push <name>:<version> <index-url>
```

The version is required. Use `uni pkg list` to see locally cached packages.

```bash
# Push a package to a remote index
uni pkg push node:20 https://packages.example.com

# Push a locally created package
uni pkg push myapp:1.2.0 https://packages.example.com
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
uni volume ls
```

Uses the global `--output` flag for format selection.

```bash
uni volume ls
# NAME     SIZE   CREATED
# mydata   2.0G   2026-04-25 18:00:00
# config   512.0M 2026-04-25 18:01:00

# JSON output
uni --output json volume ls
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

A managed network gives VMs their own bridge, subnet, gateway, and internal DNS — connect VMs to it with `uni run --network <name>`. See [Architecture]({% link architecture.md %}) for how networking is implemented.

### `uni network create`

Create a managed network. When `--subnet` is omitted, Uni auto-allocates a `/24` from `10.100.0.0/16`.

```
uni network create <name> [--subnet <cidr>] [--driver bridge]
```

| Flag | Default | Description |
|---|---|---|
| `--subnet` | *(auto-assigned)* | CIDR subnet, e.g. `10.100.0.0/24` |
| `--driver` | `bridge` | Network driver |

```bash
uni network create app
uni network create app --subnet 10.100.5.0/24
```

---

### `uni network ls`

List all managed networks.

```
uni network ls
```

```bash
uni network ls
# NAME   DRIVER   SUBNET           GATEWAY      BRIDGE
# app    bridge   10.100.0.0/24    10.100.0.1   uni-app

# JSON output
uni --output json network ls
```

---

### `uni network inspect`

Show full details for a network as JSON.

```
uni network inspect <name>
```

```json
{
  "name": "app",
  "driver": "bridge",
  "subnet": "10.100.0.0/24",
  "gateway": "10.100.0.1",
  "bridge": "uni-app",
  "created_at": "2026-04-19T10:00:00Z"
}
```

---

### `uni network rm`

Remove a managed network and its bridge.

```
uni network rm <name>
```

```bash
uni network rm app
# removed app
```

---

### `uni dns resolve`

Resolve a running VM/service name to its IP address.

```
uni dns resolve <name> [--network <name>]
```

```bash
uni dns resolve frontend --network app
uni dns resolve frontend.app
# NAME      NETWORK   IP            VM
# frontend  app       10.100.0.10   a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f
```

If the same service name exists in multiple networks, `--network` (or the `name.network` form) is required.

### `uni dns resolve-all`

Resolve **all** IP addresses registered for a service/VM name — useful for scaled services where several replicas share the same DNS name (round-robin DNS).

```
uni dns resolve-all <name> [--network <name>]
```

```bash
uni dns resolve-all backend --network app
# NAME      NETWORK   IP            VM
# backend   app       10.100.0.11   b4e9d3e2-...
# backend   app       10.100.0.12   c5f0a4b3-...
```

### `uni dns list`

List resolvable records from running VMs.

```
uni dns list [--network <name>]
```

```bash
uni dns list --network app
# NAME      NETWORK   IP            VM
# frontend  app       10.100.0.10   a3f8c2d1-...
# backend   app       10.100.0.11   b4e9d3e2-...
```

---

## Service Commands

A **service** manages a group of VM replicas running the same image as a single unit: it keeps the desired number of replicas alive, rolls out updates, and reports aggregate health. This is the same mechanism `uni compose up` uses internally whenever a compose service declares `replicas` greater than 1 — see [Compose Reference]({% link compose.md %}#scaling-with-replicas).

### `uni service run`

Create and start a service with the given number of replicas.

```
uni service run <name> <image> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--replicas` | `1` | Number of replicas to run |
| `--memory` | — | Memory per replica (e.g. `256M`) |
| `--cpus` | `0` | CPUs per replica |
| `-e`, `--env` | — | Environment variable `KEY=VALUE` (repeatable) |
| `--network` | — | Managed network for replicas (enables service DNS / round-robin resolution) |
| `--strategy` | `RollingUpdate` | Update strategy: `RollingUpdate` or `Recreate` |
| `--health-timeout` | `0` | Seconds to wait for replicas to become healthy (`0` = don't wait) |

```bash
# Start a service with 3 replicas on a managed network
uni service run backend api:latest --replicas 3 --network app -e PORT=8080

# Start with resource limits per replica
uni service run worker worker:latest --replicas 2 --memory 512M --cpus 2
```

---

### `uni service scale`

Change the number of running replicas for a service.

```
uni service scale <name> <replicas>
```

```bash
uni service scale backend 5
```

---

### `uni service update`

Roll a service over to a new image, following its configured update strategy.

```
uni service update <name> <image> [--health-timeout <seconds>]
```

| Flag | Default | Description |
|---|---|---|
| `--health-timeout` | `0` | Seconds to wait for replicas to become healthy during the update (`0` = don't wait) |

```bash
uni service update backend api:v2
```

---

### `uni service ls`

List all services.

```
uni service ls
```

```bash
uni service ls
# NAME      IMAGE        DESIRED  READY  STRATEGY        HEALTH
# backend   api:latest   3        3      RollingUpdate   healthy
# worker    worker:v1    2        1      Recreate        degraded

# JSON output
uni --output json service ls
```

---

### `uni service inspect`

Show full details for a service as JSON, including environment variables and the IDs of its replica VMs.

```
uni service inspect <name>
```

```bash
uni service inspect backend
```

```json
{
  "name": "backend",
  "image": "api:latest",
  "desired_replicas": 3,
  "ready_replicas": 3,
  "strategy": "RollingUpdate",
  "health": "healthy",
  "env": ["PORT=8080", "LOG_LEVEL=info"],
  "created_at": "2026-04-19T10:00:00Z",
  "updated_at": "2026-04-19T10:05:00Z",
  "replica_ids": [
    "a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f",
    "b4e9d3e2-8c5f-5b2g-9d3e-2b3c4d5e6f7a",
    "c5f0a4b3-9d6e-4c2f-a1b3-5d6e7f8a9b0c"
  ]
}
```

The same data is displayed as a table after `service run`, `service scale`, and `service update`:

```
Service:     backend
Image:       api:latest
Replicas:    3 desired, 3 ready
Strategy:    RollingUpdate
Health:      healthy
Created:     2026-04-19T10:00:00Z
Updated:     2026-04-19T10:05:00Z
Replicas:    [a3f8c2d1-... b4e9d3e2-... c5f0a4b3-...]
```

---

### `uni service rm`

Remove a service and stop all of its replicas.

```
uni service rm <name>
```

```bash
uni service rm backend
# removed backend
```

---

## Cluster Commands

### `uni node ls`

List cluster members with status and resource capacity.

```
uni node ls
```

**Example:**

```bash
uni node ls
# ID              ADDR             STATUS   VMS   CPU   MEM       SEEN
# a1b2c3d4        10.0.0.1:7946    alive    3     8     16.0 GiB  2026-05-16T10:00:00Z
# e5f6a7b8        10.0.0.2:7946    suspect  1     4     8.0 GiB   2026-05-16T09:55:00Z

# JSON output
uni --output json node ls
```

{: .note }
Requires `--cluster-addr` on the `unid` daemon. When cluster is disabled, `uni node ls` returns an error.

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

Download and install the latest `uni` and `unid` binaries side by side, stopping the running daemon (if any) before replacing it and restarting it afterward.

```
uni upgrade [--yes]
```

| Flag | Default | Description |
|---|---|---|
| `-y`, `--yes` | `false` | Skip confirmation prompt |

```bash
uni upgrade
# Installed CLI:  v0.1.0
# Running daemon: v0.1.0
# Latest:         v0.1.1
# New version available: v0.1.1
# Upgrade? [y/N] y
# Downloading uni...
# Downloading unid...
# uni  → /usr/local/bin/uni
# unid → /usr/local/bin/unid
# Starting new unid...
# Daemon restarted and ready.
```

{: .note }
On Windows, the running binary is renamed to `.bak` before the new one is placed in its position, since Windows does not allow overwriting a running executable directly. After the upgrade completes successfully, old `.bak` files are cleaned up automatically.

---

### `uni upgrade check`

Show the installed CLI version, the running daemon's version, and whether a newer release is available — without installing anything.

```
uni upgrade check
```

```bash
uni upgrade check
# Installed CLI:  v0.1.0
# Running daemon: v0.1.0
# Latest:         v0.1.1
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

---

## Daemon Flags

`unid` is the background daemon that manages VMs. It accepts the following flags:

| Flag | Default | Description |
|---|---|---|
| `--socket` | `/var/run/unid.sock` (Linux) / `%TEMP%\unid.sock` (Windows) | Unix socket path for VM management API |
| `--qemu` | `qemu-system-x86_64` | QEMU binary to use |
| `--store` | `~/.uni/images` | Image store root directory |
| `--vm-store` | `file` | VM state backend: `file` (per-VM JSON files) or `sqlite` (single database) |
| `--metrics-addr` | (empty, disabled) | HTTP address for Prometheus metrics (e.g. `:9090`) |
| `--ui-addr` | (empty, disabled) | HTTP address for web dashboard (e.g. `:8080`) |
| `--log-format` | `text` | Log format: `text` (default) or `json` |
| `--trace-addr` | (empty, disabled) | OTLP gRPC address for trace export (e.g. `localhost:4317`) |
| `--cluster-addr` | (empty, disabled) | HTTP address for cluster gossip endpoint (e.g. `:7946`) |
| `--join` | (empty) | Comma-separated list of seed node addresses to join (e.g. `10.0.0.2:7946`) |

### Observability Endpoints

`--metrics-addr`, `--ui-addr`, `--trace-addr`, `--log-format`, and `--vm-store` together make up Uni's observability stack — Prometheus metrics, a web dashboard, OpenTelemetry tracing, structured logging, and a SQLite-backed VM store. See the full [Observability]({% link observability.md %}) guide for endpoint lists, metric names, and setup examples; the short version:

| Flag | Enables |
|---|---|
| `--metrics-addr :9090` | `/metrics` (Prometheus) and `/health` |
| `--ui-addr :8080` | Web dashboard at `/ui` and JSON API at `/ui/api/...` |
| `--trace-addr localhost:4317` | OTLP gRPC trace export for VM lifecycle spans |
| `--log-format json` | Structured JSON log lines (`ts`, `level`, `msg`, plus contextual fields) |
| `--vm-store sqlite` | SQLite-backed VM state store instead of per-VM JSON files |

### Dashboard

When `--ui-addr` is set, `unid` serves a read-only web dashboard showing all registered VMs with their ID, name, state, health, and image — see [Observability → Web Dashboard]({% link observability.md %}#web-dashboard) for the full route and JSON API reference.

### VM Runtime Stats

`uni stats <id>` queries the daemon for runtime resource usage of a VM. Stats are collected from the QEMU process:

- **CPU%** — percentage of CPU(s) used by the VM process (Linux only, via `/proc/[pid]/stat`)
- **Memory** — resident set size in bytes (Linux only, via `/proc/[pid]/statm`)
- **Network I/O** — bytes received/transmitted on the primary interface (`eth0` or `en*`) (Linux only, via `/proc/[pid]/net/dev`)
- **Source** — `procfs` on Linux, `fallback` on other platforms

Use `--watch` for continuous monitoring:

```bash
uni stats <id> --watch
uni stats <id> --watch --interval 5s
```
