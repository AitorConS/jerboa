---
layout: default
title: Architecture
nav_order: 5
---

# Architecture
{: .no_toc }

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

## Overview

Uni is structured as a **client–daemon** system, the same model used by Docker:

```
┌─────────────────────────────────────────────────────────┐
│  uni  (CLI — short-lived process)                       │
│                                                         │
│  build · run · ps · status · logs · stop · rm · inspect · exec · cp │
│  compose up · compose down · compose ps · compose logs  │
│  service run · service scale · service update · service ls/inspect/rm │
│  volume create · volume ls · volume rm · volume inspect │
│  network create · network ls · network inspect · network rm │
│  dns resolve · dns resolve-all · dns list                │
│  node ls                                                  │
│  sign · verify                                            │
│  pkg list · pkg search · pkg get · pkg remove · pkg load│
│  kernel check · kernel update · kernel list · kernel use│
│  upgrade · upgrade check · upgrade list                 │
└──────────────────────────┬──────────────────────────────┘
                           │
                           │  JSON-RPC 2.0 over Unix domain socket
                           │  /var/run/unid.sock
                           │
┌──────────────────────────▼──────────────────────────────┐
│  unid  (daemon — long-running background process)       │
│                                                         │
│  ┌──────────────────┐  ┌──────────────────────────────┐ │
│  │   VM Manager     │  │   Image Store                │ │
│  │                  │  │   content-addressed (SHA256) │ │
│  │  QEMUManager     │  │                              │ │
│  │  ┌────────────┐  │  │   ~/.uni/images/             │ │
│  │  │ VM #1      │  │  │     <sha256>/manifest.json   │ │
│  │  │ qemu-sys.. │  │  │     <sha256>/disk.img        │ │
│  │  └────────────┘  │  │     refs.json                │ │
│  │  ┌────────────┐  │  └──────────────────────────────┘ │
│  │  │ VM #2      │  │                                   │
│  │  │ qemu-sys.. │  │  ┌──────────────────────────────┐ │
│  │  └────────────┘  │  │  Networks · Volumes · Compose│ │
│  └──────────────────┘  │  Services · Cluster gossip   │ │
│                        └──────────────────────────────┘ │
│  Observability: Prometheus · OTLP traces · dashboard ·  │
│  structured logs · file/SQLite VM store                 │
└──────────────────────────┬──────────────────────────────┘
                           │  spawns
┌──────────────────────────▼──────────────────────────────┐
│  QEMU processes  (one per running VM)                   │
│                                                         │
│  qemu-system-x86_64                                     │
│    -m 256M                                              │
│    -drive file=disk.img,format=raw,if=virtio            │
│    -nographic -serial stdio -no-reboot                  │
└──────────────────────────┬──────────────────────────────┘
                           │  boots
┌──────────────────────────▼──────────────────────────────┐
│  Nanos Kernel (C + ASM fork)                            │
│  Loads and runs the application image                   │
└─────────────────────────────────────────────────────────┘
```

---

## Components

### `uni` CLI (`cmd/uni/`)

The command-line interface. It is a **thin client** — it does no VM management itself. Every command translates directly into a JSON-RPC call to `unid`.

- One `.go` file per subcommand (`run.go`, `ps.go`, `stop.go`, ...)
- Zero business logic — just argument parsing and formatting
- Cobra framework for command routing

### `unid` daemon (`cmd/unid/`)

The long-running background process that owns everything:

- Listens on a Unix domain socket (JSON-RPC 2.0)
- Manages the VM registry (`FileStore` or `SQLiteStore`, selected with `--vm-store`)
- Spawns and monitors QEMU processes
- Owns the local image store, networks, volumes, compose state, services, and (optionally) cluster membership
- Optionally exposes Prometheus metrics, OpenTelemetry traces, a web dashboard, and structured JSON logs — see [Observability]({% link observability.md %})

### VM Manager (`internal/vm/`)

Manages the lifecycle of individual VMs:

**State machine:**
```
created → starting → running → stopping → stopped
```

Every transition is atomic (protected by `sync.RWMutex`) and logged with `slog`.

**Key types:**
- `VM` — represents one virtual machine (ID, config, state, timestamps, log buffer, health status, restart count)
- `QEMUManager` — implements the `Manager` interface by spawning `qemu-system-x86_64`
- `Store` — thread-safe registry interface for all known VMs; `MemoryStore` (in-memory), `FileStore` (JSON file persistence), or `SQLiteStore` (SQLite persistence) — selected with the daemon's `--vm-store file|sqlite` flag
- `HealthChecker` — manages TCP/HTTP probe goroutines per VM
- `RestartConfig` / `RestartPolicy` — controls automatic restart behaviour
- `RuntimeStats` / `StatsCollector` — runtime resource usage (CPU%, memory, network I/O) per VM; `ProcStatsCollector` on Linux reads `/proc/[pid]/stat`, `/proc/[pid]/statm`, `/proc/[pid]/net/dev`; `NoopStatsCollector` fallback on other platforms

**QEMU command built per VM:**
```bash
qemu-system-x86_64 \
  -m 256M \
  -drive file=/path/to/disk.img,format=raw,if=virtio \
  -nographic \
  -serial stdio \
  -no-reboot \
  -net none
```

Serial console output (stdout + stderr from QEMU) is captured into a thread-safe buffer, accessible via `uni logs`. When a VM is started with `--attach`, the output is simultaneously streamed through an `io.Pipe` so the CLI can read it in real-time via the `VM.Attach` RPC method.

### Kernel Tools Cache (`internal/tools/`)

The kernel artifacts (`kernel.img`, `boot.img`, `mkfs`, `dump`) are downloaded from GitHub releases and cached in `~/.uni/tools/`. They are versioned independently from the CLI using semver (`kernel/VERSION` in the repo).

**Download flow:**
1. `uni build` calls `tools.ResolveMkfs()`
2. `uni cp` calls `tools.ResolveDump()`
3. If tools are absent → `DownloadVersion("latest")` fetches all artifacts + saves `kernel-version.txt`
4. If tools are present → checks remote version via GitHub API; if newer, prompts `[y/N]` before replacing

**Versioned releases:** each kernel release is tagged `kernel-vX.Y.Z` on GitHub and is immutable. A rolling `latest` release always points to the most recent build. `uni kernel use <v>` downloads from the specific versioned tag.

### Image System (`internal/image/`)

**Content-addressable store** — images are stored by their SHA256 digest:

```
~/.uni/images/
  refs.json                          ← maps "name:tag" → "sha256hex"
  abc123def456.../
    manifest.json                    ← image metadata
    disk.img                         ← raw VM disk
```

**Manifest format** (`manifest.json`):
```json
{
  "schemaVersion": 1,
  "name": "hello",
  "tag": "latest",
  "created": "2026-04-19T10:00:00Z",
  "config": {
    "memory": "256M",
    "cpus": 1
  },
  "diskDigest": "sha256:abc123...",
  "diskSize": 12582912
}
```

**Builder pipeline** (`image.Builder`):
1. Validate ELF magic bytes on the binary
2. Build the Nanos boot manifest with `BuildManifest(BuildConfig)` (see below)
3. Run `mkfs` (Nanos tool) to create a raw disk image containing the binary and the boot manifest
4. Compute SHA256 of the disk
5. Write manifest.json + disk to the store

**Nanos boot manifest** (`BuildManifest`, distinct from `manifest.json` above): a tuple-format manifest passed to `mkfs` that tells the Nanos kernel how to set up the guest at boot. Built from `image.BuildConfig`:

```
(
    children:(
        node:(contents:(host:/home/user/.uni/packages/node/20.11.0/files/bin/node))
        ...
    )
    program:/program
    arguments:(0:/program 1:/server.js)
    environment:(NODE_ENV:production PYTHONPATH:/packages)
    network:(ip:10.0.2.15 gateway:10.0.2.2 netmask:255.255.255.0)
)
```

- `children` — package files and source files included in the image, nested by guest path (`pkg.File.GuestPath`)
- `arguments` — built from `BuildConfig.Entrypoint` (if set, `argv[1]` is the entrypoint script so the runtime — `node`, `python`, ... — knows what to execute) followed by `BuildConfig.Args`. For `lang = "raw"` builds, `Entrypoint` is empty and `Args` comes from `unikernel.toml`'s `[program] args`, e.g. `arguments:(0:/program 1:-jar 2:/app.jar)` for `[program] path = "java"`, `args = ["-jar", "/app.jar"]`. The `arguments` tuple is omitted entirely when both are empty
- `environment` — `BuildConfig.Env`, sorted by key. Sourced from `unikernel.toml`'s `[env]` section, the language driver (e.g. `PYTHONPATH=/packages` from the Python driver when `requirements.txt` is present), and — for `--pkg-source ops` — the `Env` field of each resolved ops package's manifest (driver values win on key conflicts)
- `network` — emitted only when `BuildConfig.Port > 0` (i.e. `uni build --port <n>`). Without it, Nanos never initializes its network stack and any HTTP server fails to bind
- Manifest tuple values use `manifestValue()` for quoting: names and values have different terminal character sets, so e.g. `/packages:/usr/lib` does not need quoting but `hello world` does

### API (`internal/api/`)

JSON-RPC 2.0 over a Unix domain socket.

**Methods:**

| Method | Description |
|---|---|
| `VM.Run` | Create + start a VM |
| `VM.Stop` | Graceful or forced stop |
| `VM.Kill` | Immediate SIGKILL |
| `VM.Signal` | Send arbitrary signal |
| `VM.Remove` | Delete a stopped VM |
| `VM.List` | List all VMs |
| `VM.Get` | Get one VM by ID |
| `VM.Logs` | Get captured serial output (snapshot) |
| `VM.Attach` | Stream serial console output in real-time |
| `VM.Inspect` | Full VM details |
| `VM.Stats` | Runtime resource usage (CPU, memory, network) |
| `Network.Create/List/Get/Remove` | Manage named networks |
| `Network.AllocateIP/ReleaseIP` | IPAM allocation lifecycle |
| `DNS.Resolve` | Resolve a service/VM name to an IP on a network |
| `DNS.ResolveAll` | Resolve a name to every matching IP (for replica round-robin) |
| `DNS.List` | List active DNS records |
| `Service.Run` | Create and start a replicated service |
| `Service.Scale` | Change a service's replica count |
| `Service.Update` | Roll out a new image/config across a service's replicas |
| `Service.List/Get/Remove` | Manage known services |
| `Node.List` | List cluster members (requires `--cluster-addr`) |
| `Daemon.Version` | Report the daemon's version |
| `Daemon.Shutdown` | Gracefully stop the daemon |

### Compose (`internal/compose/`)

Parses compose YAML files and resolves startup order:

- **Parser** — validates schema (version, service images, dependency refs, network refs)
- **Graph** — Kahn's topological sort algorithm with cycle detection

### Package System (`internal/package/`)

Two independent package ecosystems are supported, selected with `--source`/`--pkg-source uni|ops` (see [Package Commands]({% link cli-reference.md %}#package-commands)):

**`uni` native index** — pre-packaged files for inclusion in images at build time:

- **Store** — local cache at `~/.uni/packages/<name>/<version>/` holding:
  - `files.tar.gz` — the downloaded package archive
  - `files/` — extracted contents of the archive
  - `meta.json` — package metadata (name, version, SHA256, etc.)
- **FetchIndex** — retrieves the remote package index listing available packages and versions
- **Download** — fetches the package archive from its URL and stores it locally (with size verification)
- **Extract** — decompresses `files.tar.gz` into the `files/` subdirectory
- **ExtractedFiles** — lists all regular files inside the extracted package
- **Search** — queries the remote index by name, description, or runtime
- **Get** — downloads a package (optionally a specific version) to the local store
- **Remove** — deletes a specific version; **RemoveAll** — deletes all versions of a package
- **Create** — builds a local package archive from a binary and optional additional files, computing SHA256 and writing `meta.json`

Packages are included at build time via `uni build --pkg <name>[:<version>]`. The build pipeline:

1. `resolvePackages()` fetches the remote index and resolves each `--pkg` reference
2. Downloads the archive (`files.tar.gz`) if not already cached
3. Extracts the archive into `files/` if not already extracted
4. Collects all individual file paths from `files/` via `ExtractedFiles()`
5. Passes the file list to `buildManifest()` which includes each file in the Nanos manifest

**`ops` ecosystem (`OpsStore`)** — pre-built language runtimes from the `nanovms`/`eyberg` package hub at `repo.ops.city`, used both by `uni pkg` (with `--source ops`) and as the runtime source for source-based builds (`uni build --pkg-source ops`):

- **`OpsStore`** — local cache at `~/.uni/packages-ops/<namespace>/<name>/<version>/`, mirroring the layout the upstream `ops` tool expects
- **`FetchOpsManifest`/`FetchManifestCached`** — downloads (and caches) the manifest at `repo.ops.city/v2/manifest.json`, listing every available `<namespace>/<name>:<version>` package with its language, architecture, and SHA256
- **`Lookup`** — resolves a `<namespace>/<name>[:<version>]` reference against the manifest, normalizing `v` prefixes and matching version prefixes (e.g., a query of `11` matches `v11.5.0`)
- **`Download`/`Extract`/`FindBinary`** — fetches the package archive (verifying its SHA256), extracts it, and locates the runtime binary inside it
- **`lookupOpsPackage`** — used by `uni build --pkg-source ops` to resolve an unqualified runtime name (e.g., `node`) by searching the namespaces `eyberg`, `nanovms`, and `myuniverse`, in that order, for the closest version match to the project's declared runtime version (e.g., `engines.node` in `package.json`)

This is the mechanism behind [building directly from source]({% link getting-started.md %}#2b-or-build-directly-from-source): the language driver detects the project's runtime requirement, resolves a matching `ops` package, downloads and extracts it, and bundles the runtime binary into the image alongside the compiled application.

### Environment Variable Injection

Environment variables passed via `uni run -e KEY=VALUE` reach the guest through QEMU's `fw_cfg` device — no disk rebuild required.

**Flow:**
1. `uni run -e KEY=VAL` → daemon builds `-fw_cfg name=opt/uni/env,string=KEY=VAL\n`
2. QEMU exposes this as a named file on the fw_cfg device (I/O ports `0x510`/`0x511`)
3. At boot, `env_inject_from_fw_cfg()` in the kernel reads `opt/uni/env` and merges entries into the process environment tuple before `exec_elf` builds the user-space stack

This is x86-64 only; the function compiles to a no-op stub on aarch64.

### Network Configuration Injection

Static IP configuration passed via `uni run --ip` reaches the guest through QEMU's `fw_cfg` device, the same mechanism used for environment variables.

**Flow:**
1. `uni run --network app --ip 10.0.0.2` → daemon builds `-fw_cfg name=opt/uni/network,string=10.0.0.2/24,10.0.0.1`
2. QEMU exposes this as a named file on the fw_cfg device (I/O ports `0x510`/`0x511`)
3. At boot, `net_inject_from_fw_cfg()` in the kernel reads `opt/uni/network`, parses the IP/CIDR and gateway, and injects them into the root tuple
4. `init_network_iface()` picks up the injected values to configure the first ethernet interface with a static IP instead of DHCP

The format is `IP/CIDR,GATEWAY` (e.g. `10.0.0.2/24,10.0.0.1`). This is x86-64 only.

---

## File Copy (`uni cp`)

`uni cp` copies files to and from stopped VM disk images using the `dump` and `mkfs` tools from the Nanos kernel toolchain. The tools read and write the TFS (Tiny File System) filesystem directly on the raw disk image.

**Copy FROM a VM** — the `dump` tool extracts the entire filesystem to a temporary directory, then the requested file is copied to the destination.

**Copy TO a VM** — the `dump` tool extracts the filesystem, the new file is injected, then `mkfs` rebuilds the disk image with the updated content.

**Download flow:**
1. `uni cp` calls `tools.ResolveDump()` (and `tools.ResolveMkfs()` for copy-to-VM)
2. If tools are absent from `~/.uni/tools/` → `downloadArtifact()` fetches them from the latest kernel release
3. For copy-from: extract filesystem, copy file to host
4. For copy-to: extract filesystem, copy file in, rebuild disk with `mkfs`

This requires the VM to be in `stopped` state because the disk image must not be in use by a running QEMU process.

---

## Networking

Each VM can use one of two networking modes, chosen automatically based on the flags passed to `uni run`:

**SLIRP user-mode** (used when `-p` is set without `--network`): QEMU's built-in user-mode networking with port forwarding via `hostfwd` rules. Works on any platform without root access. Does not support inbound ICMP (ping).

**TAP + bridge** (used when `--network <name>` is set): `--network` takes the name of a *managed network* created beforehand with `uni network create` — not a raw host interface name. The daemon looks it up via `Network.Get`, creates a TAP interface, and bridges it on the Linux host, giving the VM full network access including its own IP address (auto-allocated from the network's subnet, or pinned with `--ip`). Requires Linux and elevated permissions (`CAP_NET_ADMIN`/root). When port mappings (`-p`) are used together with `--network`, iptables DNAT rules are automatically configured so that traffic arriving at the host is forwarded to the guest's static IP. The bridge is created via `internal/network/bridge_linux.go`, the TAP is attached, and iptables rules (with interface filtering via `-i tapName`) are applied for port forwarding. When `--ip` is specified, the guest-side static IP is configured via fw_cfg (`opt/uni/network`) — no DHCP required.

{: .note }
TAP networking requires Linux and elevated permissions. It is not available on Windows. See `internal/network/tap.go` (Linux-only build tag).

---

## Health Checks

VMs can be configured with liveness probes that run periodically after startup:

- **TCP probe** — succeeds if a TCP connection can be established to the guest port
- **HTTP probe** — succeeds if an HTTP GET to the guest port/path returns a 2xx status code

**Configuration** (via `--health-check` flag or API):

| Parameter | Default | Description |
|---|---|---|
| Type | — | `tcp` or `http` |
| Port | — | Guest port to probe (maps to host port via PortMaps if set) |
| Path | `/` | HTTP path (only for `http` type) |
| Interval | 10s | Time between probes |
| Timeout | 3s | Per-probe timeout |
| Retries | 3 | Consecutive failures before marking `unhealthy` |

**Probe target resolution**: when `PortMaps` are configured, the probe targets the host-side port. Otherwise it targets the guest port directly on `127.0.0.1`.

**Health States:**

| State | Meaning |
|---|---|
| `starting` | Probe period not yet elapsed |
| `healthy` | Last probe succeeded |
| `unhealthy` | Consecutive failures exceeded `Retries` |
| `unknown` | No health check configured |

---

## Restart Policies

When a VM exits (crashes or terminates), the daemon can automatically restart it:

| Policy | Behavior |
|---|---|
| `never` | Never restart (default) |
| `on-failure` | Restart only on non-zero exit code |
| `always` | Always restart, even on clean exit (unless explicitly stopped) |

**Configuration** (via `--restart` flag or API):

```
--restart never              # never restart (default)
--restart on-failure         # restart on crash (unlimited retries)
--restart on-failure:5       # restart on crash, max 5 retries
--restart always             # always restart (unlimited retries)
--restart always:3           # always restart, max 3 retries
```

**Exponential backoff** between restarts: 1s, 2s, 4s, 8s, 16s, capped at 30s.

**Important:** `StateStopped` is terminal — the restart creates a **new VM** with the same Config. The old VM is removed from the store and the new VM gets a fresh ID and incremented `RestartCount`.

Explicit stop operations (`uni stop` or `uni kill`) set an `explicitStop` flag that prevents restart regardless of policy.

---

## Web Dashboard

The daemon serves a read-only web dashboard when `--ui-addr` is configured (e.g. `--ui-addr :8080`).

### Pages

| Route | Description |
|---|---|
| `/ui` | VM list with state, health, image |
| `/ui/vm/{id}` | VM detail: config, health, restart info, port mappings, env vars, serial console log tail |

### JSON API Endpoints

| Endpoint | Description |
|---|---|
| `/ui/api/vms` | List all VMs (id, name, state, image, health) |
| `/ui/api/vm/{id}` | Full VM detail as JSON |
| `/ui/api/vm/{id}/logs` | Serial console output for a VM |
| `/ui/api/vm/{id}/stats` | Live runtime stats (CPU%, memory, network I/O) |

The dashboard uses Go HTML templates with a dark theme. No JavaScript framework is required. VM IDs in the list are clickable links to the detail page. The detail page polls stats every 3 seconds via the `/ui/api/vm/{id}/stats` endpoint.

---

## Resource Quotas

VMs can have CPU and memory limits enforced via Linux cgroup v2 when available.

### CPU Shares

The `--cpu-shares` flag sets the cgroup v2 CPU weight for the QEMU process:

```bash
uni run myapp:latest --cpu-shares 512
```

CPU weight ranges from 1 to 10000 (default 100). This controls relative CPU allocation among competing VMs, not an absolute limit.

### Memory Hard Limit

The `--memory-max` flag sets a cgroup v2 memory hard limit:

```bash
uni run myapp:latest --memory-max 512M
```

When the QEMU process exceeds this limit, the kernel OOM killer will terminate it. Supported suffixes: `K`, `M`, `G`.

### Platform Requirements

Both features require Linux with cgroup v2 (`/sys/fs/cgroup/cgroup.controllers` must exist). On non-Linux platforms, the flags are accepted but no limits are enforced and a warning is logged.

The daemon creates a cgroup at `/sys/fs/cgroup/uni/<vm-id>/` for each VM with resource limits, moves the QEMU PID into it on start, and removes the cgroup on VM exit.

---

## I/O Throttling

Disk I/O for the boot disk can be limited using QEMU's native drive throttle:

```bash
# Limit to 1000 IOPS
uni run myapp:latest --disk-iops 1000

# Limit to 10MB/s throughput
uni run myapp:latest --disk-bps 10M

# Both limits
uni run myapp:latest --disk-iops 500 --disk-bps 5M
```

| Flag | Unit | Description |
|---|---|---|
| `--disk-iops` | IOPS | Maximum I/O operations per second (0 = no limit) |
| `--disk-bps` | bytes/sec | Maximum throughput (e.g. `10M`, `1G`; 0 = no limit) |

These limits apply to the boot disk only. Volume disks are not throttled.

---

## Cluster Membership

When started with `--cluster-addr`, `unid` joins a SWIM-style gossip cluster for node discovery and health monitoring.

**How it works:**

Each daemon runs a lightweight gossip protocol over HTTP:

1. **Join** — on startup, contacts seed nodes listed in `--join` and exchanges membership tables
2. **Gossip** — every 5 seconds, picks a random peer and exchanges membership state via `POST /cluster/gossip`
3. **Suspicion** — if a member is not heard from for 15 seconds, it is marked `suspect`
4. **Dead** — if a suspect is not heard from for 30 seconds, it is marked `dead`
5. **Leave** — on graceful shutdown, the local node broadcasts its `left` status

**Member states:**

| State | Meaning |
|---|---|
| `alive` | Active and responding to gossip |
| `suspect` | Not heard from recently (may be network issue) |
| `dead` | Not heard from for an extended period |
| `left` | Gracefully shut down |

Dead and Left statuses are always propagated regardless of timestamp, ensuring cluster-wide consistency.

**Usage:**

```bash
# Start first node
unid --cluster-addr :7946

# Start second node, joining the first
unid --cluster-addr :7946 --join 10.0.0.1:7946

# Start with multiple seeds
unid --cluster-addr :7946 --join 10.0.0.1:7946,10.0.0.2:7946

# List cluster members
uni node ls
```

`0.0.0.0` bind addresses are normalized to `127.0.0.1` for inter-node communication.

---

## Security Model

- `unid` runs as root (or a privileged user) to spawn QEMU and manage TAP interfaces
- The Unix socket is the trust boundary — only processes that can access the socket file can manage VMs
- Each VM runs in full KVM hardware isolation — a compromised unikernel cannot escape to the host or other VMs
- No shell, no SSH, no dynamic linking inside the unikernel — attack surface is minimal by design
