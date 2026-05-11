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
│  volume create · volume ls · volume rm · volume inspect │
│  network create · network ls · network inspect · network rm │
│  dns resolve · dns list                                  │
│  pkg list · pkg search · pkg get · pkg remove           │
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
│  │   VM Manager     │  │   Image Registry (HTTP)      │ │
│  │                  │  │                              │ │
│  │  QEMUManager     │  │   GET  /v2/images            │ │
│  │  ┌────────────┐  │  │   POST /v2/images            │ │
│  │  │ VM #1      │  │  │   GET  /v2/images/{ref}      │ │
│  │  │ qemu-sys.. │  │  │   GET  /v2/images/{ref}/disk │ │
│  │  └────────────┘  │  │   DELETE /v2/images/{ref}    │ │
│  │  ┌────────────┐  │  └──────────────────────────────┘ │
│  │  │ VM #2      │  │                                   │
│  │  │ qemu-sys.. │  │  ┌──────────────────────────────┐ │
│  │  └────────────┘  │  │   Image Store                │ │
│  └──────────────────┘  │   ~/.uni/images/             │ │
│                        │   <sha256>/manifest.json     │ │
│                        │   <sha256>/disk.img          │ │
│                        │   refs.json                  │ │
│                        └──────────────────────────────┘ │
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
│  Loads and runs the static ELF application              │
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
- Manages the VM registry (in-memory `Store`)
- Spawns and monitors QEMU processes
- Optionally serves the HTTP image registry

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
- `Store` — thread-safe registry interface for all known VMs; `MemoryStore` for in-memory, `FileStore` for JSON persistence
- `HealthChecker` — manages TCP/HTTP probe goroutines per VM
- `RestartConfig` / `RestartPolicy` — controls automatic restart behaviour

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
2. Run `mkfs` (Nanos tool) to create a raw disk image containing the binary
3. Compute SHA256 of the disk
4. Write manifest + disk to the store

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
| `Network.Create/List/Get/Remove` | Manage named networks |
| `Network.AllocateIP/ReleaseIP` | IPAM allocation lifecycle |
| `DNS.Resolve` | Resolve service/VM names to IP |
| `DNS.List` | List active DNS records |

### Compose (`internal/compose/`)

Parses compose YAML files and resolves startup order:

- **Parser** — validates schema (version, service images, dependency refs, network refs)
- **Graph** — Kahn's topological sort algorithm with cycle detection

### Package System (`internal/package/`)

Manages pre-packaged files that can be included in images at build time:

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

Packages are included at build time via `uni build --pkg <name>[:<version>]`. The build pipeline:

1. `resolvePackages()` fetches the remote index and resolves each `--pkg` reference
2. Downloads the archive (`files.tar.gz`) if not already cached
3. Extracts the archive into `files/` if not already extracted
4. Collects all individual file paths from `files/` via `ExtractedFiles()`
5. Passes the file list to `buildManifest()` which includes each file in the Nanos manifest

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
1. `uni run --ip 10.0.0.2 --network tap0` → daemon builds `-fw_cfg name=opt/uni/network,string=10.0.0.2/24,10.0.0.1`
2. QEMU exposes this as a named file on the fw_cfg device (I/O ports `0x510`/`0x511`)
3. At boot, `net_inject_from_fw_cfg()` in the kernel reads `opt/uni/network`, parses the IP/CIDR and gateway, and injects them into the root tuple
4. `init_network_iface()` picks up the injected values to configure the first ethernet interface with a static IP instead of DHCP

The format is `IP/CIDR,GATEWAY` (e.g. `10.0.0.2/24,10.0.0.1`). This is x86-64 only.

---

## Image Registry

When started with `--registry-addr :5000`, `unid` serves an HTTP registry.

Current behavior is hybrid:
- Legacy API under `/v2/images` is still available for backward compatibility.
- OCI v2 foundations are available under `/v2/...` for blob upload/download and manifest put/get/delete.
- OCI blobs are persisted in `~/.uni/blobs`.
- OCI manifest refs/bodies are persisted in `~/.uni/oci` and survive daemon restarts.

```
GET    /v2/images                          list all images (legacy)
GET    /v2/images/{ref}                    get manifest (legacy)
GET    /v2/images/{ref}/disk               download raw disk image (legacy)
POST   /v2/images                          push image multipart (legacy)
DELETE /v2/images/{ref}                    remove image (legacy)

GET    /v2/                                OCI API base (200 when available)
GET    /v2/_catalog                        list OCI repositories
POST   /v2/{name}/blobs/uploads/           start OCI blob upload
PUT    /v2/{name}/blobs/uploads/{uuid}     complete OCI blob upload
GET    /v2/{name}/blobs/{digest}           download OCI blob
HEAD   /v2/{name}/blobs/{digest}           check OCI blob existence + digest
DELETE /v2/{name}/blobs/{digest}           delete OCI blob
PUT    /v2/{name}/manifests/{ref}          store OCI manifest ref
GET    /v2/{name}/manifests/{ref}          read OCI manifest ref
HEAD   /v2/{name}/manifests/{ref}          check OCI manifest existence + digest
DELETE /v2/{name}/manifests/{ref}          delete OCI manifest ref
```

Full OCI compliance/auth/signing is tracked in Phase 8, but the migration path is active:
`uni push/pull` use OCI first and fall back to legacy endpoints if needed.

Registry auth is now available as an optional static bearer token gate:

- Start daemon with `--registry-token <token>` (or `UNI_REGISTRY_TOKEN=<token>`)
- When enabled, registry endpoints require `Authorization: Bearer <token>`
- Unauthorized requests return `401` with `WWW-Authenticate: Bearer realm="uni-registry"`

Scoped JWT auth is also available for registry endpoints:

- Start daemon with `--registry-jwt-secret <secret>` (or `UNI_REGISTRY_JWT_SECRET=<secret>`)
- Optional claim checks can be configured with `--registry-jwt-issuer` / `UNI_REGISTRY_JWT_ISSUER` and `--registry-jwt-audience` / `UNI_REGISTRY_JWT_AUDIENCE`
- Tokens are validated as HMAC JWTs and must include a `scope` claim
- Supported scope format is Docker-style: `repository:<name>:pull,push` (supports `*` repo wildcard)
- Missing/invalid tokens return `401`; valid tokens without required action scope return `403`

Registry HTTPS can be enabled with custom certificate files:

- Start daemon with `--registry-tls-cert <path>` and `--registry-tls-key <path>`
- Environment alternatives: `UNI_REGISTRY_TLS_CERT` and `UNI_REGISTRY_TLS_KEY`
- Both cert and key are required together; partial TLS config is rejected at startup

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

Each VM can use one of two networking modes:

**SLIRP user-mode** (default for `-p`): QEMU's built-in user-mode networking with port forwarding via `hostfwd` rules. Works on any platform without root access. Does not support inbound ICMP (ping).

**TAP + bridge**: A TAP interface is created and bridged on the Linux host, giving the VM full network access including its own IP address. Requires Linux and elevated permissions. When port mappings (`-p`) are used together with `--network`, iptables DNAT rules are automatically configured so that traffic arriving at the host is forwarded to the guest's static IP. The bridge is created via `internal/network/bridge_linux.go`, the TAP is attached, and iptables rules (with interface filtering via `-i tapName`) are applied for port forwarding. When `--ip` is specified, the guest-side static IP is configured via fw_cfg (`opt/uni/network`) — no DHCP required.

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

## Security Model

- `unid` runs as root (or a privileged user) to spawn QEMU and manage TAP interfaces
- The Unix socket is the trust boundary — only processes that can access the socket file can manage VMs
- Each VM runs in full KVM hardware isolation — a compromised unikernel cannot escape to the host or other VMs
- No shell, no SSH, no dynamic linking inside the unikernel — attack surface is minimal by design
