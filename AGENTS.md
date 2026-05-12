# AGENTS.md — Unikernel Engine

> Docker-like unikernel engine. Forks Nanos (C+ASM kernel), adds Go orchestration layer.
> Stack: Go 1.22+, C, ASM on KVM/QEMU.

## Build Commands

```bash
make build              # compile all Go binaries (uni + unid)
make kernel             # build Nanos fork (requires cross-compiler)
make test               # unit tests
make test-integration   # integration tests (requires KVM: /dev/kvm)
make lint               # golangci-lint
make e2e                # full end-to-end suite
make coverage           # HTML coverage report
```

Single test:
```bash
go test ./internal/vm/... -run TestVMStart -v
go test -tags integration ./tests/integration/... -run TestBoot -timeout 10m
```

## Architecture

```
uni CLI (cobra) → Unix socket → unid daemon → KVM/QEMU wrapper
                                           → Registry client
                                           → Scheduler/orchestrator
                 Nanos kernel (C+ASM fork) ← image loader

unireg (standalone registry server) → OCI/legacy HTTP API with auth/TLS
```

**CLI (`cmd/uni/`)** — one file per subcommand, cobra, zero business logic, all work delegated to `unid` via Unix socket. Always has `--output json` flag. Subcommands: `run`, `build`, `images`, `rmi`, `push`, `pull`, `ps`, `status`, `logs`, `stop`, `rm`, `inspect`, `exec`, `compose`, `volume`, `network`, `dns`, `kernel`, `pkg`, `cp`, `upgrade`.

**Daemon (`cmd/unid/`)** — persistent process, Unix socket API (JSON-RPC 2.0), cluster-aware scheduling. Creates `~/.uni/networks/` Network Store on startup. Registry server can be embedded via `--registry-addr`.

**Registry (`cmd/unireg/`)** — standalone registry server with same OCI/legacy API, auth, TLS, and GC as the embedded daemon registry. Independently deployable. Uses `--addr`, `--token`, `--jwt-secret`, `--tls-cert`/`--tls-key`, `--no-auto-tls` flags.

**API (`internal/api/`)** — JSON-RPC 2.0 over Unix domain socket. Methods: `VM.Run`, `VM.Stop`, `VM.Kill`, `VM.Signal`, `VM.Remove`, `VM.List`, `VM.Get`, `VM.Logs`, `VM.Attach`, `VM.Inspect`, `Network.Create`, `Network.List`, `Network.Get`, `Network.Remove`, `Network.AllocateIP`, `Network.ReleaseIP`, `DNS.Resolve`, `DNS.List`.

**VM Manager (`internal/vm/`)** — KVM/QEMU wrapper. `VM` struct is concurrent-safe (`sync.RWMutex`). State machine: `created → starting → running → stopping → stopped`. KVM ioctls wrapped in testable interfaces — never call ioctls directly in business logic.

**Image System (`internal/image/`)** — custom JSON manifest + raw disk image, content-addressable by SHA256. `uni build` validates ELF magic bytes, runs `mkfs`, computes SHA256, writes to `~/.uni/images/<sha256>/`. `BuildManifest()` constructs the Nanos manifest including package files from `--pkg`.

**Package System (`internal/package/`)** — manages pre-packaged runtime files for `uni build --pkg`. Pipeline: `FetchIndex()` → `Download()` (SHA-256 verified) → `Extract()` (tar.gz) → `ExtractedFiles()` (file list for manifest). Local store at `~/.uni/packages/<name>/<version>/` with `files.tar.gz`, `files/`, `meta.json`. `IndexURL` is a `var` (overridable in tests). `RemoveAll()` deletes all versions; `Remove()` deletes one.

**Registry (`internal/registry/`)** — hybrid HTTP registry with legacy and OCI flows. Legacy endpoints: `GET /v2/images`, `GET /v2/images/{ref}`, `GET /v2/images/{ref}/disk`, `POST /v2/images`, `DELETE /v2/images/{ref}`. OCI endpoints: `/v2/`, `/v2/_catalog`, blob upload/download/delete, manifest put/get/delete. Optional auth via static bearer token (`--registry-token` / `UNI_REGISTRY_TOKEN`) or scoped JWT (`--registry-jwt-secret` / `UNI_REGISTRY_JWT_SECRET`) with optional issuer/audience validation (`--registry-jwt-issuer`, `--registry-jwt-audience`), plus optional HTTPS with custom cert/key (`--registry-tls-cert` / `--registry-tls-key`). Auto-generates self-signed cert at `~/.uni/registry/tls/` when registry is enabled without custom TLS.

**Volume System (`internal/volume/`)** — named persistent virtio-blk disks at `~/.uni/volumes/<name>/disk.img`. Sparse files via seek+write. Created with `uni volume create`, mounted with `uni run -v name:/guest/path[:ro]`. Survive VM restarts.

**Compose (`internal/compose/`)** — YAML parser + validator. Topological sort via Kahn's algorithm with cycle detection. Writes `.uni-compose-state.json` alongside compose file: `{"project": "...", "services": {"frontend": "<vm-id>", "backend": "<vm-id>"}, "service_networks": {"frontend": "app"}, "service_ips": {"frontend": "10.100.0.2"}, "created_networks": ["mynet"]}`. Networks section creates/destroys bridges on `compose up`/`compose down`. Services support `health_check` (tcp/http probes) and `restart` (never/on-failure/always[:N]) directives.

**Kernel Tools (`internal/tools/`)** — auto-downloads `mkfs`, `kernel.img`, `boot.img` from GitHub releases to `~/.uni/tools/`. Handles version checking and updates. Platform-specific mkfs resolution.

**Kernel (`kernel/`)** — Nanos fork, C+ASM only. Never touch C from Go directly. Always boot-test changes in QEMU. Add C tests under `kernel/test/` for any new kernel function.

## Key Technical Decisions

| Area | Choice |
|---|---|
| KVM interface | QEMU process wrapper initially; migrate to `/dev/kvm` ioctls in Phase 3+ |
| IPC | Unix domain socket, JSON-RPC 2.0 |
| Logging | `slog` (stdlib) in Go; kernel serial console captured by daemon |
| Config | TOML (daemon), JSON (manifests), YAML (compose) |
| DI | Manual constructor injection — no framework |
| Image format | JSON manifest + raw disk, SHA256 content-addressable |
| Networking | TAP + Linux bridge with IPAM; `~/.uni/networks/<name>/` store; dynamic `uni-br-<name>` bridges; auto IP allocation from /24 subnets in 10.100.0.0/16 |

## Code Rules

- All errors wrapped with context: `fmt.Errorf("starting vm %s: %w", id, err)`
- No global mutable state — constructor injection only
- Interfaces over concrete types in function signatures
- Functions under 50 lines; extract helpers aggressively
- Every exported symbol needs a godoc comment
- All state transitions logged with `slog`

## Testing

- Unit tests co-located: `internal/vm/vm_test.go`
- Integration tests in `tests/integration/`, tagged `//go:build integration`
- CLI tests in `cmd/uni/` use in-process daemon (`startDaemon`), fake QEMU, `httptest.NewServer` for registry and package index
- Overrideable test vars: `pkg.IndexURL` (package index URL), `pkgStoreDir` (package store path)
- Use `testify/require` (fail fast), `gomock`/`mockery` for mocks
- Table-driven tests for all parser/validator logic
- Target 80%+ coverage on `internal/` and `pkg/`
- Integration tests require self-hosted KVM runner

## CI (GitHub Actions)

| Workflow | Triggers | What it does |
|---|---|---|
| `pr.yml` | PRs to `main` | lint + unit tests + kernel build + integration tests (self-hosted KVM runner) |
| `main.yml` | Push to `main` | lint + unit tests + E2E (TODO: enable) + multi-arch release builds + GitHub Release |
| `kernel-release.yml` | Changes to `kernel/**` | builds kernel + mkfs, publishes versioned tag + rolling `latest` release |
| `nightly.yml` | Daily 02:00 UTC | kernel tests + benchmarks + govulncheck + trivy + failure notification (TODO: webhook) |
| `docs.yml` | Changes to `docs/` | Jekyll build + GitHub Pages deploy |

Self-hosted runner needed for `integration-tests` (`runs-on: [self-hosted, linux, kvm]`). When `/dev/kvm not found`, fix with `sudo usermod -aG kvm $USER` then restart runner.

## Phase Status

Currently in **Phase 8** (Registry & Distribution) — OCI/auth/TLS/GC/search/signing/autotls implemented; Docker CLI validation and unireg split complete.

| Phase | Status | Key deliverables |
|---|---|---|
| 0 — Foundation | ✅ done | Nanos fork, CI green, QEMU boots |
| 1 — VM Manager | ✅ done | State machine, QEMU wrapper, Unix socket API, `uni run` |
| 2 — Image System | ✅ done | Manifest, content-addressable store, registry, `uni build/images/rmi/push/pull` |
| 3 — Full CLI | ✅ done | `uni ps/logs/stop/rm/inspect/exec`, `--output json`, 81% cmd/uni coverage |
| 4 — Compose | ✅ done | YAML parser, topological sort, shared volumes, `uni compose up/down/ps/logs` |
| 5 — Complete Runtime | ✅ done | Port mapping, env vars, volumes, named instances, `--attach`, `--ip` (host+guest fw_cfg), `uni cp` (to+from VM), `uni volume`, TAP/bridge networking |
| 6 — Package System | ✅ done | `uni pkg list/search/get/remove`, `--pkg` flag on `uni build`, package index/store, archive extraction, `internal/package/` |
| 7.0 — Stabilization | ✅ done | Test coverage (tools, cp, kernel, upgrade, unid tests), `httpclient` 30s timeout, `CommandFunc` context, `MockManager`, lint fixes |
| 7.1 — Persistence | ✅ done | `Store` interface, `FileStore` with `state.json`, `Restore()` on daemon startup, `DaemonRecovered` flag |
| 7.2 — Health Checks | ✅ done | `HealthChecker` (TCP/HTTP probes), `--health-check` flag, `HealthStatus` on VM/VMDetail, configurable interval/timeout/retries |
| 7.3 — Auto-Restart | ✅ done | `RestartPolicy` (never/on-failure/always), `--restart` flag, exponential backoff, `RestartCount`, `explicitStop` tracking |
| 7.4 — Service/Status | ✅ done | `uni status` command, health/restart columns in `uni ps`, `RestartSpec` in API |
| 7.5 — IPAM + Networks | ✅ done | Network Store + IPAM (`internal/network/store.go`), `uni network create/ls/inspect/rm`, dynamic bridges (`uni-br-<name>`), `uni run --network <name>` auto-allocates IP, compose network integration, JSON-RPC `Network.*` endpoints |
| 7.6 — DNS | ✅ done | Internal DNS resolver in `unid` (`DNS.Resolve`/`DNS.List`), scoped names (`name.network`), ambiguity detection, and `uni dns` CLI |
| 7.7 — Integration | ✅ done | Compose health checks (`health_check:`) and restart policies (`restart:`), wait-for-healthy in `compose up`, parser validation, AGENTS.md update |
| 8 — Registry & Distribution | ✅ done | OCI-compatible registry with auth/TLS/GC/search/signing/autotls; standalone `unireg` service; `uni sign`/`uni verify` |
| 9 — Build System | ⬜ | Multi-language `uni build` (Go/Node/Python/Rust), `unikernel.toml`, multi-arch |
| 10 — Observability | ⬜ | Prometheus metrics, web dashboard, multi-node cluster, daemon persistence |

Phases must be fully tested and stable before advancing. A phase is not done if tests are skipped, lint fails, or only the happy path works.

## Known Platform Notes

- `Stop()` (graceful) sends SIGTERM → 30s → SIGKILL. On Windows SIGTERM is unsupported; falls back to SIGKILL immediately.
- `isFilePath()` handles Windows drive-letter paths (`C:\...`) in addition to Unix prefixes.
- TAP networking (`internal/network/tap.go`) is `//go:build linux` only.
- Non-Linux builds include `internal/network/tap_stub.go` so TAP symbols compile cross-platform and fail with explicit runtime errors.
- Bridge creation (`internal/network/bridge_linux.go`) is `//go:build linux` only.
- `parseSig()` uses integer literals for SIGUSR1/SIGUSR2 (`syscall.Signal(10/12)`) for cross-platform compatibility.
- `volume.ParseSize` uses `strconv.ParseInt` (not `fmt.Sscanf`) — Sscanf accepts trailing junk like `"1X"` silently.
- `gofmt` rejects trailing-spaces alignment in struct literals. When CI flags gofmt, run `gofmt -w` directly rather than guessing the alignment.
- `pkg.IndexURL` is a `var` (not `const`) so tests can override it to point at `httptest.NewServer`.
- `pkgStoreDir` in `cmd/uni/pkg.go` is a package-level `var` that overrides `pkgStorePath()` in tests — set it to `t.TempDir()` and restore in `t.Cleanup()`.
- `Download()` in `internal/package/` closes the file handle before `os.Remove` on error — Windows cannot delete an open file.
- `uni pkg remove <name>` (without version) calls `RemoveAll()` which deletes all locally cached versions of that package.
- Health check probes (`internal/vm/health.go`) use background context with timeouts; cancelled probe goroutines are cleaned up in `HealthChecker.Stop()`.
- Restart policy `always` restarts on any exit (including clean shutdown) unless `Stop()` or `Kill()` was called, which sets `explicitStop`. `on-failure` only restarts on non-zero exit code. `never` (default) never restarts.
- `restartVM()` creates a NEW VM with the same Config — `StateStopped` is terminal, the old VM is removed from the store and replaced.
- Exponential backoff: 1s, 2s, 4s, 8s, 16s, capped at 30s. Controlled by `RestartCount` on the VM.
- Network Store persists in `~/.uni/networks/<name>/` with `meta.json` (Network struct) and `state.json` (allocated IPs). IPAM assigns from `.2` upward; gateway is always `.1`.
- `uni network create <name>` auto-allocates a `/24` from `10.100.0.0/16` if `--subnet` is not specified. Bridges are named `uni-br-<name>`.
- `uni run --network <name>` resolves the network, auto-allocates an IP via IPAM, and passes `BridgeName`/`SubnetMask`/`GatewayIP` to the daemon.
- Compose `networks:` section creates networks on `compose up` and removes them on `compose down`. Services with `networks:` get auto-allocated IPs.
- Internal DNS resolves only running VMs with `NetworkName` + `IPAddress`; duplicate names across networks require explicit scope (`--network` or `name.network`).

## CLI Subcommands

| Command | Flags | Description |
|---|---|---|
| `uni run <image>` | `--memory`, `-p/--port`, `-e/--env`, `--env-file`, `--name`, `--rm`, `-v/--volume`, `--attach`, `-d/--detach`, `--ip`, `--network`, `--health-check`, `--restart`, `--verify` | Create and start a unikernel VM |
| `uni build` | `--name`, `--tag`, `--pkg` | Build a unikernel image |
| `uni images` | — | List local images |
| `uni rmi` | — | Remove a local image |
| `uni push` | — | Push image to registry |
| `uni pull` | `--verify` | Pull image from registry |
| `uni search <registry>/<query>` | — | Search remote registry repositories |
| `uni sign <image>` | `--key` | Sign a local image with Ed25519 key |
| `uni verify <image>` | — | Verify image signature |
| `uni ps` | — | List running VMs |
| `uni status` | — | Show VM summary with health/restart info |
| `uni logs <id>` | — | Show captured serial console output |
| `uni stop <id>` | `--force` | Stop (or kill) a VM |
| `uni rm <id>` | — | Remove a stopped VM |
| `uni inspect <id>` | — | Full VM detail as JSON |
| `uni exec <id> <cmd>` | — | Execute command in VM |
| `uni compose up/down/ps/logs` | `--volumes` | Multi-service orchestration |
| `uni volume create/ls/rm/inspect` | — | Manage persistent volumes |
| `uni network create/ls/inspect/rm` | `--subnet`, `--driver` | Manage networks |
| `uni dns resolve/list` | `--network` | Resolve and inspect internal VM DNS records |
| `uni run --network <name>` | `--network`, `--ip` | Auto-allocate IP from network |
| `uni kernel check/update/list/use` | — | Manage kernel tools |
| `uni pkg list/search/get/remove` | — | Manage packages |
| `uni cp <src> <dst>` | — | Copy files to/from VM |
| `uni upgrade` | — | Self-update CLI binary |

Build pipeline in `internal/vm/qemu.go::buildCmd`:
- Network priority: `NetworkName` (TAP) > `PortMaps` non-empty (SLIRP `hostfwd`) > `-net none`.
- SLIRP user-mode (`-netdev user,...,hostfwd=tcp::8080-:80`) does not need TAP/bridge or root, works on any platform — preferred for `-p`.
- Env vars are passed via `-fw_cfg name=opt/uni/env,string=KEY=VAL\n…`. The kernel reads this at boot.
- Network config (static IP) is passed via `-fw_cfg name=opt/uni/network,string=IP/MASK,GATEWAY`. Format uses `Config.SubnetMask` (not hardcoded `/24`): `10.0.0.2/24,10.0.0.1`.
- Bridge and TAP interfaces use dynamic names from the network store: bridge = `uni-br-<network-name>`, TAP remains as `Config.NetworkName`.
- Volumes attach as extra `-drive file=...,format=raw,if=virtio,index=N` after the boot disk (index 0).

## Kernel Patches (uni-specific additions to Nanos fork)

- **`kernel/src/drivers/fw_cfg.{c,h}`** — QEMU fw_cfg driver, x86-only (uses I/O ports `0x510`/`0x511`). Reads named files (e.g. `opt/uni/env`) by walking the directory at entry `0x0019`. Confirms `"QEMU"` signature before use; safe no-op on bare metal.
- **`kernel/src/unix/env_inject.c`** — `env_inject_from_fw_cfg(root)` reads `opt/uni/env` and merges entries into `root[environment]` tuple. Called from `stage3.c::startup()` before `exec_elf` builds the user stack envp. Compiles on aarch64 too (`#ifdef __x86_64__` guards the body to a stub).
- **`kernel/src/unix/net_inject.c`** — `net_inject_from_fw_cfg(root)` reads `opt/uni/network` and injects static IP configuration (`ipaddr`, `netmask`, `gateway`) into root tuple. `init_network_iface()` picks this up to configure the first ethernet interface instead of DHCP. x86-only (fw_cfg dependency).
- When changing kernel boot order or the manifest tuple structure, the fw_cfg call site is in `kernel/src/kernel/stage3.c::startup` right after `init_management_root` / `init_kernel_heaps_management`. Must run before `exec_elf` reads the environment tuple.

## Versioning

Both the CLI and the kernel are independently versioned with semver.

| Component | Version file | Release tag format | Pipeline |
|---|---|---|---|
| CLI (uni/unid) | `VERSION` | `v0.1.0` | `main.yml` |
| Kernel artifacts | `kernel/VERSION` | `kernel-v0.1.0` | `kernel-release.yml` |

**Rules:**
- Bump `VERSION` before every commit that changes CLI code.
- Bump `kernel/VERSION` before every commit that changes `kernel/`.
- Patch bump (`0.1.0 → 0.1.1`) for fixes; minor bump (`0.1.0 → 0.2.0`) for features.
- Each pipeline publishes an immutable versioned release **and** updates the shared rolling `latest` release, uploading only its own assets (CLI pipeline never touches kernel assets and vice versa).

**Kernel tools cache** (`~/.uni/tools/`):
- `uni build` auto-downloads kernel artifacts on first use via `internal/tools.ResolveMkfs`.
- `uni build` checks for a newer kernel version before building and prompts `[y/N]`.
- `uni kernel check` / `uni kernel update` / `uni kernel list` / `uni kernel use <v>` manage the cached kernel version.
- After bumping `kernel/VERSION` and pushing, wait for `kernel-release.yml` to complete before the new kernel is available to download.

**CLI self-update:**
- `uni upgrade` replaces the running `uni` binary (and `unid` if found alongside it).
- `uni upgrade check` / `uni upgrade list` for inspection without installing.
- Windows: renames the running binary to `.bak` before placing the new one (cannot overwrite a running `.exe` directly).

## Registry Service (`unireg`)

`unireg` is a standalone registry server extracted from `unid`. It provides the same OCI and legacy HTTP API with auth, TLS, and GC capabilities.

**Usage:**

```bash
# Start registry with auto-generated self-signed TLS on :5000
unireg

# Start registry without TLS
unireg --no-auto-tls

# Start with custom TLS cert and JWT auth
unireg --tls-cert /path/to/cert.pem --tls-key /path/to/key.pem --jwt-secret mysecret

# Start on custom address
unireg --addr :8080

# Garbage collect unreferenced blobs
unireg gc
```

**Flags:** `--addr`, `--store`, `--token`, `--jwt-secret`, `--jwt-issuer`, `--jwt-audience`, `--tls-cert`, `--tls-key`, `--no-auto-tls`

**Environment variables:** `UNI_REGISTRY_TOKEN`, `UNI_REGISTRY_JWT_SECRET`, `UNI_REGISTRY_JWT_ISSUER`, `UNI_REGISTRY_JWT_AUDIENCE`, `UNI_REGISTRY_TLS_CERT`, `UNI_REGISTRY_TLS_KEY`

## Repository Notes

- Default branch: `main`. No `develop` branch despite some workflow references.
- Remote: `AitorConS/UniCli` (renamed). Pushes work but emit a redirect notice — not a hook failure.

## Critical Function/File Index

| What | Where |
|---|---|
| `uni run` flag wiring | `cmd/uni/run.go` |
| Daemon RPC dispatch | `internal/api/server.go::dispatch` |
| QEMU command builder | `internal/vm/qemu.go::buildCmd` + `buildNetArgs`/`buildEnvArgs`/`buildNetworkCfgArgs`/`buildVolumeArgs` |
| Port spec parser | `internal/vm/portmap.go::ParsePortMap` |
| Compose YAML validators | `internal/compose/parser.go::validatePortSpec` / `validateVolumeSpec` |
| Volume disk allocation | `internal/volume/volume.go::allocateDisk` (sparse via seek+write) |
| Kernel envp construction | `kernel/src/unix/exec.c::build_exec_stack` (reads `process_root[environment]`) |
| Boot-time env injection | `kernel/src/kernel/stage3.c::startup` calls `env_inject_from_fw_cfg(root)` |
| Boot-time network injection | `kernel/src/kernel/stage3.c::startup` calls `net_inject_from_fw_cfg(root)` |
| Kernel tools download/cache | `internal/tools/mkfs.go::ResolveMkfs` + `internal/tools/version.go` |
| Kernel version check (build) | `cmd/uni/build.go::checkKernelUpdateForBuild` |
| Network config fw_cfg | `internal/vm/qemu.go::buildNetworkCfgArgs` — uses `Config.SubnetMask` (not hardcoded `/24`); format: `IP/MASK,GW` |
| Host-side bridge/TAP | `internal/network/bridge_linux.go` — `CreateBridge`, `AttachTAP`, `DestroyBridge`; bridge name from `Config.BridgeName` (not hardcoded) |
| Network Store + IPAM | `internal/network/store.go` — `Store` with `Create/Get/List/Remove/AllocateIP/ReleaseIP`; persistent `~/.uni/networks/<name>/` with `meta.json` + `state.json`; subnet allocator from 10.100.0.0/16 |
| iptables port forwarding | `internal/network/portfwd_linux.go` — DNAT + MASQUERADE with `-i tapName` |
| Package index/store | `internal/package/package.go` — `Store`, `FetchIndex`, `Search`, `Extract`, `ExtractedFiles`, `RemoveAll` |
| Package download with SHA-256 | `internal/package/package.go::Download` — verifies `Package.SHA256` after download, removes archive on mismatch; skips when empty |
| `uni pkg` commands | `cmd/uni/pkg.go` — list, search, get, remove (all versions) |
| Package resolution (build) | `cmd/uni/build.go::resolvePackages` — download, extract, list files for manifest |
| Manifest with package files | `internal/image/builder.go::BuildManifest` — includes extracted package files as manifest children |
| `uni pkg` CLI tests | `cmd/uni/pkg_test.go` — search, get, list, remove, remove-all-versions, not-found, parsePkgRef |
| `resolvePackages` tests | `cmd/uni/resolve_test.go` — download→extract→list pipeline, specific version, not-found, multiple packages |
| Package pipeline integration test | `tests/integration/package_pipeline_test.go` — full Download→Extract→ExtractedFiles→BuildManifest end-to-end |
| `uni cp` (to VM) | `cmd/uni/cp.go::cpToVM` — dump → copy file → mkfs rebuild |
| Compose shared volumes | `internal/compose/types.go::VolumeConfig` + `cmd/uni/compose.go::newComposeUpCmd` |
| CLI self-update | `cmd/uni/upgrade.go::replaceBinary` |
| CLI version (injected at build) | `cmd/uni/main.go::version` — set via `-X main.version` in `main.yml` |
| Image signing and verification | `internal/signing/signing.go` — Ed25519 key generation, signing, verification, key store (`~/.uni/keys/`) |
| `uni sign` / `uni verify` | `cmd/uni/sign.go` — sign local images, verify signatures; `--verify` flag on `uni run` and `uni pull` |
| Auto-generated self-signed TLS | `internal/autotls/autotls.go` — RSA 2048-bit key + X.509 cert, 365 days validity, stored at `~/.uni/registry/tls/`, reused on subsequent starts |
| Standalone registry | `cmd/unireg/main.go` — independently deployable registry server with same API/auth/TLS/GC as embedded daemon registry |
| Docker compatibility tests | `tests/integration/docker_compat_test.go` — validates Docker CLI patterns against registry server |
| Health check probes | `internal/vm/health.go` — `HealthChecker`, TCP/HTTP probes, backoff, `probeTarget` |
| Restart policy logic | `internal/vm/qemu.go::monitor` — evaluates `RestartConfig` on process exit, calls `restartVM` with backoff |
| Restart policy CLI flag | `cmd/uni/run.go::parseRestartPolicy` — `--restart never/on-failure/always[:N]` |
| Health check CLI flag | `cmd/uni/run.go::parseHealthCheck` — `--health-check tcp:PORT/http:PORT:/path` |
| VM persistence | `internal/vm/filestore.go` — `FileStore` with `state.json`, `Restore()` on daemon startup |
| VM status command | `cmd/uni/status.go` — `uni status` shows VM summary with health/restart info |
| Network CLI | `cmd/uni/network.go` — `uni network create/ls/inspect/rm`, `--subnet` and `--driver` flags |
| Network config auto-IP | `cmd/uni/run.go` — `--network <name>` resolves network from store, auto-allocates IP via IPAM |
| Compose network integration | `cmd/uni/compose.go` — creates networks in `compose up`, assigns IPs to services, removes in `compose down` |
| Compose health checks | `cmd/uni/compose.go` — `health_check:` field in compose services, mapped to `api.HealthCheckSpec`, wait-for-healthy in `compose up` |
| Compose restart policies | `cmd/uni/compose.go` — `restart:` field in compose services, mapped to `api.RestartSpec` |
| Compose YAML validation | `internal/compose/parser.go` — `validateHealthCheckSpec`, `validateRestartSpec` |

## Internal Packages

| Package | Description |
|---|---|
| `internal/api/` | JSON-RPC 2.0 server/client over Unix socket. VM lifecycle RPC methods. |
| `internal/compose/` | Compose YAML parser, validator, Kahn's topological sort with cycle detection, shared volumes. |
| `internal/image/` | Image build pipeline (ELF validation, mkfs, SHA256, `BuildManifest` with package files) + content-addressable store. |
| `internal/network/` | TAP device + Linux bridge setup, iptables port forwarding (Linux-only), **Network Store + IPAM** (`store.go`) with persistent `~/.uni/networks/<name>/` directories. Network type with subnet allocator (10.100.0.0/16 → /24 blocks), AllocateIP/ReleaseIP, bridge-per-network convention (`uni-br-<name>`). |
| `internal/package/` | Package index fetch, local store, download (SHA-256 verified), extract (tar.gz), search, remove. |
| `internal/registry/` | Hybrid registry server/client with legacy `/v2/images` and OCI `/v2/...` flows, persistent OCI blobs/manifests, optional bearer/JWT auth, and optional registry TLS. |
| `internal/signing/` | Ed25519 image signing and verification. Key pair generation and storage at `~/.uni/keys/`. Signature files stored alongside manifests (`manifest.json.sig`). Verification policy: `off` (default), `warn` (log warnings), `enforce` (fail on missing/invalid). |
| `internal/autotls/` | Auto-generation of self-signed TLS certificates for the registry. Generates RSA 2048-bit key + X.509 cert valid 365 days, stored at `~/.uni/registry/tls/`. Reuses existing certs on subsequent starts. |
| `internal/scheduler/` | DNS resolver for name-to-IP lookups over running VMs (Phase 7.6). |
| `internal/tools/` | Kernel tools management: download, version check, platform-specific mkfs resolution. |
| `internal/vm/` | Core package: VM lifecycle state machine, QEMU wrapper, port map parser, VM registry store, network cfg via fw_cfg, health checks, restart policies, persistence. |
| `internal/volume/` | Named volume management: sparse disk creation, attach/detach as virtio-blk devices. |

## Stub Packages (placeholders for future phases)

| Path | Phase | Purpose |
|---|---|---|
| `pkg/` | 6+ | Public shared libraries |
| `tests/unit/` | — | Empty; unit tests are co-located with source files |

## Session Handoff (2026-05-08)

### Completed Today

- Added internal DNS resolver package (`internal/scheduler/resolver.go`) and tests (`internal/scheduler/resolver_test.go`).
- Added JSON-RPC DNS methods (`DNS.Resolve`, `DNS.List`) in API server/client (`internal/api/server.go`, `internal/api/client.go`, `internal/api/types.go`) with API tests.
- Added `uni dns resolve` and `uni dns list` CLI commands (`cmd/uni/dns.go`) with tests (`cmd/uni/dns_test.go`).
- Updated compose state to persist per-service network/IP (`service_networks`, `service_ips`) and use them during `compose down` IP release.
- Extended coverage for `cmd/uni/run.go` helpers, `cmd/uni/kernel.go`, and `cmd/uni/upgrade.go` with focused tests.

### Coverage Snapshot

- `internal/api`: 73.2%
- `internal/tools`: 72.0%
- `internal/scheduler`: 90.9%
- `cmd/uni`: 64.9%

### Next Steps (Tomorrow)

1. Raise `internal/tools` coverage from 72% toward 80%:
   - extend `version_test.go` and add failure-path tests for artifact download/save logic.
2. Add compose/network integration tests for IP release stability across `compose up/down/up` cycles.
3. Start Phase 8 planning doc split:
   - OCI compatibility scope,
   - image signing strategy,
   - JWT auth boundaries.

### Validation Commands

- `go test ./...`
- `go test -cover ./internal/api/... ./internal/scheduler/... ./internal/tools/... ./cmd/uni/...`
- `golangci-lint run --timeout 5m ./...`

## Session Update (2026-05-10)

### Completed

- Added non-Linux TAP stubs in `internal/network/tap_stub.go` to make TAP API compile safely on all platforms.
- Expanded `internal/tools/mkfs_test.go` with `downloadArtifact()` success and failure-path tests (HTTP errors, request build error, write/create-dir failures, cancelled context).
- Expanded `internal/registry/server_test.go` with remove and bad-payload cases for `/v2/images` handlers.
- Added OCI foundation types in `internal/ociregistry/types.go` with parser/validator tests.
- Added content-addressable blob store foundation in `internal/ociblob/store.go` with CRUD/dedup tests.
- Added initial OCI registry HTTP routes in `internal/registry/server.go` (`/v2/_catalog`, blob upload start/complete/get/delete, manifest put/get/delete).
- Added initial OCI client flows in `internal/registry/client.go` (`PushOCI`/`PullOCI`) with layer tar+gzip packing/unpacking.
- Wired `unid` registry startup to pass an OCI blob store (`~/.uni/blobs`) via `registry.WithBlobStore`.
- Added OCI integration tests in `internal/registry/oci_test.go` covering v2 base/catalog, blob upload, manifest roundtrip, and digest mismatch.
- Added persistent OCI manifest storage in `internal/registry/ocistore.go` (`~/.uni/oci`) and wired it into the registry server via `registry.WithOCIStore`.
- Updated `uni push/pull` to prefer OCI flows (`PushOCI`/`PullOCI`) with automatic fallback to legacy `/v2/images` endpoints.
- Added persistence coverage to OCI integration tests (`manifest survives server restart` case).

### Next Validation

- `go test ./internal/tools ./internal/registry ./internal/network`
- `go test -cover ./internal/tools/... ./internal/registry/...`

## Session Update (2026-05-11)

### Completed

- Added optional bearer auth to registry server via `registry.WithBearerToken`.
- Wired daemon registry auth flags/env: `unid --registry-token` and `UNI_REGISTRY_TOKEN`.
- Updated OCI base endpoint behavior: `GET /v2/` returns `200` when available; when auth is enabled, unauthenticated requests receive `401` with `WWW-Authenticate` challenge.
- Extended registry client auth propagation so legacy and OCI requests both send bearer tokens when configured.
- Added auth coverage in `internal/registry/oci_test.go` and updated base endpoint expectations.

### Validation

- `go test ./internal/registry ./cmd/unid`

## Session Update (2026-05-11, OCI upload chunks)

### Completed

- Added OCI chunk upload endpoint support in `internal/registry/server.go` via `PATCH /v2/<name>/blobs/uploads/<uuid>`.
- Updated blob upload completion to consume chunked upload state + final PUT body when present.
- Added integration coverage in `internal/registry/oci_test.go` for `POST` -> `PATCH` -> `PUT` upload flow.

### Validation

- `go test ./internal/registry ./cmd/unid`

## Session Checkpoint (2026-05-11, end-of-day)

### Phase 8 Progress Saved

- Registry now supports: OCI-first push/pull, nested repositories, HEAD endpoints, Docker-style auth challenges, bearer + JWT auth (with issuer/audience checks), registry TLS (custom cert/key), CLI auth/TLS flags, remote search, and safe blob GC.
- Documentation and roadmap have been aligned to reflect current state and remaining work.

### Remaining to close Phase 8

- Image signing and verification (`8.2`, `8.3`).
- Registry self-signed bootstrap path (`8.5` remainder).
- Final Docker CLI interoperability validation (`8.10` remainder).

## Session Update (2026-05-11, CLI registry e2e)

### Completed

- Added CLI integration tests in `cmd/uni/uni_test.go` for registry auth+TLS flows covering `uni push`, `uni pull`, and `uni search`.
- Added secure test registry helper using `httptest.NewTLSServer` with optional OCI stores and bearer auth.
- Validated that global CLI registry auth/TLS flags are effective in end-to-end command execution paths.

### Validation

- `go test ./cmd/uni`

## Session Update (2026-05-11, OCI nested repos)

### Completed

- Improved OCI request routing in `internal/registry/server.go` to support nested repository names (e.g. `team/app`) across blobs and manifests endpoints.
- Updated repo extraction logic used by scoped JWT authorization to work with nested OCI repository paths.
- Added nested repository roundtrip coverage in `internal/registry/oci_test.go` (`PushOCI` + manifest GET + `PullOCI`).

### Validation

- `go test ./internal/registry`

## Session Update (2026-05-11, Docker auth challenge)

### Completed

- Added Docker-style `WWW-Authenticate` challenge format in registry auth responses (`realm`, `service`, and repo/action `scope` when applicable).
- Kept `GET /v2/` challenge unscoped while manifest/blob unauthorized requests include repository-scoped pull/push actions.
- Added integration coverage in `internal/registry/oci_test.go` for challenge header behavior.

### Validation

- `go test ./internal/registry ./cmd/unid`

## Session Update (2026-05-11, JWT claims)

### Completed

- Added optional JWT issuer/audience validation in registry auth via `registry.WithJWTValidation`.
- Wired daemon flags/env: `--registry-jwt-issuer` / `UNI_REGISTRY_JWT_ISSUER` and `--registry-jwt-audience` / `UNI_REGISTRY_JWT_AUDIENCE`.
- Extended JWT integration coverage with issuer/audience allow/deny checks in `internal/registry/oci_test.go`.
- Extended `cmd/unid/main_test.go` flag coverage for JWT claim validation flags.

### Validation

- `go test ./internal/registry ./cmd/unid`

## Session Update (2026-05-11, registry TLS)

### Completed

- Added registry HTTPS support in daemon via `--registry-tls-cert` / `--registry-tls-key`.
- Wired registry TLS env vars: `UNI_REGISTRY_TLS_CERT` and `UNI_REGISTRY_TLS_KEY`.
- Added startup validation that requires cert+key together when TLS is configured.
- Extended daemon flag/test coverage in `cmd/unid/main_test.go` for TLS flags and config validation.

### Validation

- `go test ./cmd/unid ./internal/registry`

## Session Update (2026-05-11, CLI registry auth/tls)

### Completed

- Added global CLI registry auth flag/env: `--registry-token` / `UNI_REGISTRY_TOKEN`.
- Added global CLI TLS options: `--registry-ca-cert` / `UNI_REGISTRY_CA_CERT` and `--registry-insecure` / `UNI_REGISTRY_INSECURE`.
- Extended registry client TLS support with custom CA trust and optional insecure TLS mode.
- Wired `uni push` and `uni pull` to consistently apply registry auth/TLS options for both OCI and legacy fallback flows.

### Validation

- `go test ./cmd/uni ./internal/registry`

## Session Update (2026-05-11, registry GC)

### Completed

- Added registry GC engine in `internal/registry/gc.go` that removes unreferenced blobs while preserving manifest-referenced config/layers.
- Added digest reference enumeration in `internal/registry/ocistore.go` via `ReferencedDigests()`.
- Added `unid gc` command to run registry blob GC from the daemon binary.
- Added GC coverage in `internal/registry/gc_test.go` and command presence coverage in `cmd/unid/main_test.go`.

### Validation

- `go test ./internal/registry ./cmd/unid`

## Session Update (2026-05-11, registry search)

### Completed

- Added `uni search <registry>/<query>` in `cmd/uni/search.go` using OCI catalog (`/v2/_catalog`) with case-insensitive substring filtering.
- Added registry client catalog support via `ListRepositories()` in `internal/registry/client.go`.
- Wired `uni search` to the same global registry auth/TLS options used by `uni push`/`uni pull`.
- Added parser coverage for `<registry>/<query>` input in `cmd/uni/search_test.go`.

### Validation

- `go test ./cmd/uni ./internal/registry`

## Session Update (2026-05-11, JWT auth)

### Completed

- Added optional JWT auth to registry server via `registry.WithJWTAuth`.
- Wired daemon registry JWT auth flags/env: `unid --registry-jwt-secret` and `UNI_REGISTRY_JWT_SECRET`.
- Enforced repo/action scope checks from JWT `scope` claim using Docker-style entries (`repository:<name>:pull,push`) with wildcard support.
- Added integration coverage in `internal/registry/oci_test.go` for JWT scope allow/deny behavior.

### Validation

- `go test ./internal/registry ./cmd/unid`

## Session Update (2026-05-11, follow-up)

### Completed

- Added OCI `HEAD` endpoints for blobs and manifests in `internal/registry/server.go`.
- `HEAD /v2/<name>/blobs/<digest>` now returns existence + `Docker-Content-Digest` without streaming the blob body.
- `HEAD /v2/<name>/manifests/<ref>` now returns existence + digest headers for both in-memory and persistent OCI manifest stores.
- Extended OCI integration coverage in `internal/registry/oci_test.go` for both new `HEAD` flows.

### Validation

- `go test ./internal/registry ./cmd/unid`
