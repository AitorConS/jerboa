# AGENTS.md â€” Unikernel Engine

> Docker-like unikernel engine. Forks Nanos (C+ASM kernel), adds Go orchestration layer.
> Stack: Go 1.25+, C, ASM on KVM/QEMU.

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
uni CLI (cobra) â†’ Unix socket â†’ unid daemon â†’ KVM/QEMU wrapper
                                           â†’ Registry client
                                           â†’ Scheduler/orchestrator
                 Nanos kernel (C+ASM fork) â† image loader

unireg (standalone registry server) â†’ OCI/legacy HTTP API with auth/TLS
```

**CLI (`cmd/uni/`)** â€” one file per subcommand, cobra, zero business logic, all work delegated to `unid` via Unix socket. Always has `--output json` flag. Subcommands: `run`, `build`, `images`, `rmi`, `push`, `pull`, `ps`, `status`, `logs`, `stop`, `rm`, `inspect`, `exec`, `compose`, `volume`, `network`, `dns`, `kernel`, `pkg`, `cp`, `upgrade`, `stats`.

**Daemon (`cmd/unid/`)** â€” persistent process, Unix socket API (JSON-RPC 2.0), cluster-aware scheduling via SWIM gossip. Creates `~/.uni/networks/` Network Store on startup. Registry server can be embedded via `--registry-addr`. Cluster membership via `--cluster-addr` and `--join` flags.

**Registry (`cmd/unireg/`)** â€” standalone registry server with same OCI/legacy API, auth, TLS, and GC as the embedded daemon registry. Independently deployable. Uses `--addr`, `--token`, `--jwt-secret`, `--tls-cert`/`--tls-key`, `--no-auto-tls` flags.

**API (`internal/api/`)** â€” JSON-RPC 2.0 over Unix domain socket. Methods: `VM.Run`, `VM.Stop`, `VM.Kill`, `VM.Signal`, `VM.Remove`, `VM.List`, `VM.Get`, `VM.Logs`, `VM.Attach`, `VM.Inspect`, `VM.Stats`, `Network.Create`, `Network.List`, `Network.Get`, `Network.Remove`, `Network.AllocateIP`, `Network.ReleaseIP`, `DNS.Resolve`, `DNS.List`, `Node.List`.

**VM Manager (`internal/vm/`)** â€” KVM/QEMU wrapper. `VM` struct is concurrent-safe (`sync.RWMutex`). State machine: `created â†’ starting â†’ running â†’ stopping â†’ stopped`. KVM ioctls wrapped in testable interfaces â€” never call ioctls directly in business logic.

**Image System (`internal/image/`)** â€” custom JSON manifest + raw disk image, content-addressable by SHA256. `uni build` validates ELF magic bytes, runs `mkfs`, computes SHA256, writes to `~/.uni/images/<sha256>/`. `BuildManifest()` constructs the Nanos manifest including package files from `--pkg`.

**Package System (`internal/package/`)** â€” manages pre-packaged runtime files for `uni build --pkg`. Pipeline: `FetchIndex()` â†’ `Download()` (SHA-256 verified) â†’ `Extract()` (tar.gz) â†’ `ExtractedFiles()` (file list for manifest). Local store at `~/.uni/packages/<name>/<version>/` with `files.tar.gz`, `files/`, `meta.json`. `IndexURL` is a `var` (overridable in tests). `RemoveAll()` deletes all versions; `Remove()` deletes one.

**Registry (`internal/registry/`)** â€” hybrid HTTP registry with legacy and OCI flows. Legacy endpoints: `GET /v2/images`, `GET /v2/images/{ref}`, `GET /v2/images/{ref}/disk`, `POST /v2/images`, `DELETE /v2/images/{ref}`. OCI endpoints: `/v2/`, `/v2/_catalog`, blob upload/download/delete, manifest put/get/delete. Optional auth via static bearer token (`--registry-token` / `UNI_REGISTRY_TOKEN`) or scoped JWT (`--registry-jwt-secret` / `UNI_REGISTRY_JWT_SECRET`) with optional issuer/audience validation (`--registry-jwt-issuer`, `--registry-jwt-audience`), plus optional HTTPS with custom cert/key (`--registry-tls-cert` / `--registry-tls-key`). Auto-generates self-signed cert at `~/.uni/registry/tls/` when registry is enabled without custom TLS.

**Volume System (`internal/volume/`)** â€” named persistent virtio-blk disks at `~/.uni/volumes/<name>/disk.img`. Sparse files via seek+write. Created with `uni volume create`, mounted with `uni run -v name:/guest/path[:ro]`. Survive VM restarts.

**Compose (`internal/compose/`)** â€” YAML parser + validator. Topological sort via Kahn's algorithm with cycle detection. Writes `.uni-compose-state.json` alongside compose file: `{"project": "...", "services": {"frontend": "<vm-id>", "backend": "<vm-id>"}, "service_networks": {"frontend": "app"}, "service_ips": {"frontend": "10.100.0.2"}, "created_networks": ["mynet"]}`. Networks section creates/destroys bridges on `compose up`/`compose down`. Services support `health_check` (tcp/http probes) and `restart` (never/on-failure/always[:N]) directives.

**Kernel Tools (`internal/tools/`)** â€” auto-downloads `mkfs`, `kernel.img`, `boot.img` from GitHub releases to `~/.uni/tools/`. Handles version checking and updates. Platform-specific mkfs resolution.

**Kernel (`kernel/`)** â€” Nanos fork, C+ASM only. Never touch C from Go directly. Always boot-test changes in QEMU. Add C tests under `kernel/test/` for any new kernel function.

## Key Technical Decisions

| Area | Choice |
|---|---|
| KVM interface | QEMU process wrapper initially; migrate to `/dev/kvm` ioctls in Phase 3+ |
| IPC | Unix domain socket, JSON-RPC 2.0 |
| Logging | `slog` (stdlib) in Go; kernel serial console captured by daemon; `--log-format text|json` switches between `slog.TextHandler` and custom `slogformat.JSONHandler` |
| Tracing | OpenTelemetry OTLP gRPC export; `--trace-addr` enables; no-op when empty |
| Dashboard | Go-served HTML templates on `--ui-addr`; no JS framework; `/ui/api/vms` JSON endpoint; VM detail page at `/ui/vm/{id}` with log tail + live stats polling; `/ui/api/vm/{id}`, `/ui/api/vm/{id}/logs`, `/ui/api/vm/{id}/stats` JSON endpoints |
| Config | TOML (daemon), JSON (manifests), YAML (compose) |
| DI | Manual constructor injection â€” no framework |
| Image format | JSON manifest + raw disk, SHA256 content-addressable |
| Networking | TAP + Linux bridge with IPAM; `~/.uni/networks/<name>/` store; dynamic `uni-br-<name>` bridges; auto IP allocation from /24 subnets in 10.100.0.0/16 |

## Code Rules

- All errors wrapped with context: `fmt.Errorf("starting vm %s: %w", id, err)`
- No global mutable state â€” constructor injection only
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
| `main.yml` | Push to `main` | lint + unit tests + E2E + multi-arch release builds + GitHub Release |
| `kernel-release.yml` | Changes to `kernel/**` | builds kernel + mkfs, publishes versioned tag + rolling `latest` release |
| `nightly.yml` | Daily 02:00 UTC | kernel tests + benchmarks + govulncheck + trivy (fail on HIGH/CRITICAL) + failure notification (requires `NOTIFY_WEBHOOK` secret) |
| `docs.yml` | Changes to `docs/` | Jekyll build + GitHub Pages deploy |

Self-hosted runner needed for `integration-tests` (`runs-on: [self-hosted, linux, kvm]`). When `/dev/kvm not found`, fix with `sudo usermod -aG kvm $USER` then restart runner.

CI uses Go 1.25 in workflows; golangci-lint pinned to v2.12.2 with v2 config format.

## Phase Status

Currently in **Phase 10** (Observability & Production Hardening) â€” all items complete. Phase 10 is done. CI uses Go 1.25; golangci-lint pinned to v2.12.2.

| Phase | Status | Key deliverables |
|---|---|---|
| 0 â€” Foundation | âœ… done | Nanos fork, CI green, QEMU boots |
| 1 â€” VM Manager | âœ… done | State machine, QEMU wrapper, Unix socket API, `uni run` |
| 2 â€” Image System | âœ… done | Manifest, content-addressable store, registry, `uni build/images/rmi/push/pull` |
| 3 â€” Full CLI | âœ… done | `uni ps/logs/stop/rm/inspect/exec`, `--output json`, 81% cmd/uni coverage |
| 4 â€” Compose | âœ… done | YAML parser, topological sort, shared volumes, `uni compose up/down/ps/logs` |
| 5 â€” Complete Runtime | âœ… done | Port mapping, env vars, volumes, named instances, `--attach`, `--ip`, `uni cp`, TAP/bridge networking |
| 6 â€” Package System | âœ… done | `uni pkg list/search/get/remove/create`, `--pkg` flag, package index/store, archive extraction |
| 7 â€” Orchestrator | âœ… done | Health checks, restart policies, status, DNS, network/IPAM, compose integration (7.0â€“7.7) |
| 8 â€” Registry & Distribution | âœ… done | OCI registry, auth/JWT/TLS, signing, `unireg`, search, GC |
| 9 â€” Build System | âœ… done | Build Driver framework, 4 language drivers, `unikernel.toml`, `.unignore`, build cache, `--platform` |
| 10 â€” Observability | âœ… done | Prometheus âœ…, JSON logging âœ…, OTel tracing âœ…, `uni stats` âœ…, dashboard âœ…, SQLite persistence âœ…, resource quotas âœ…, I/O throttling âœ…, cluster membership âœ…, `uni node ls` âœ… |

Phases must be fully tested and stable before advancing. A phase is not done if tests are skipped, lint fails, or only the happy path works.

## Known Platform Notes

- `Stop()` (graceful) sends SIGTERM â†’ 30s â†’ SIGKILL. On Windows SIGTERM is unsupported; falls back to SIGKILL immediately.
- `isFilePath()` handles Windows drive-letter paths (`C:\...`) in addition to Unix prefixes.
- TAP networking (`internal/network/tap.go`) is `//go:build linux` only.
- Non-Linux builds include `internal/network/tap_stub.go` so TAP symbols compile cross-platform and fail with explicit runtime errors.
- Bridge creation (`internal/network/bridge_linux.go`) is `//go:build linux` only.
- `parseSig()` uses integer literals for SIGUSR1/SIGUSR2 (`syscall.Signal(10/12)`) for cross-platform compatibility.
- `volume.ParseSize` uses `strconv.ParseInt` (not `fmt.Sscanf`) â€” Sscanf accepts trailing junk like `"1X"` silently.
- `gofmt` rejects trailing-spaces alignment in struct literals. When CI flags gofmt, run `gofmt -w` directly rather than guessing the alignment.
- `pkg.IndexURL` is a `var` (not `const`) so tests can override it to point at `httptest.NewServer`.
- `pkgStoreDir` in `cmd/uni/pkg.go` is a package-level `var` that overrides `pkgStorePath()` in tests â€” set it to `t.TempDir()` and restore in `t.Cleanup()`.
- `Download()` in `internal/package/` closes the file handle before `os.Remove` on error â€” Windows cannot delete an open file.
- `uni pkg remove <name>` (without version) calls `RemoveAll()` which deletes all locally cached versions of that package.
- Health check probes (`internal/vm/health.go`) use background context with timeouts; cancelled probe goroutines are cleaned up in `HealthChecker.Stop()`.
- Restart policy `always` restarts on any exit (including clean shutdown) unless `Stop()` or `Kill()` was called, which sets `explicitStop`. `on-failure` only restarts on non-zero exit code. `never` (default) never restarts.
- `restartVM()` creates a NEW VM with the same Config â€” `StateStopped` is terminal, the old VM is removed from the store and replaced.
- Exponential backoff: 1s, 2s, 4s, 8s, 16s, capped at 30s. Controlled by `RestartCount` on the VM.
- Network Store persists in `~/.uni/networks/<name>/` with `meta.json` (Network struct) and `state.json` (allocated IPs). IPAM assigns from `.2` upward; gateway is always `.1`.
- `uni network create <name>` auto-allocates a `/24` from `10.100.0.0/16` if `--subnet` is not specified. Bridges are named `uni-br-<name>`.
- `uni run --network <name>` resolves the network, auto-allocates an IP via IPAM, and passes `BridgeName`/`SubnetMask`/`GatewayIP` to the daemon.
- Compose `networks:` section creates networks on `compose up` and removes them on `compose down`. Services with `networks:` get auto-allocated IPs.
- Internal DNS resolves only running VMs with `NetworkName` + `IPAddress`; duplicate names across networks require explicit scope (`--network` or `name.network`).

## CLI Subcommands

| Command | Flags | Description |
|---|---|---|
| `uni run <image>` | `--memory`, `-p/--port`, `-e/--env`, `--env-file`, `--name`, `--rm`, `-v/--volume`, `--attach`, `-d/--detach`, `--ip`, `--network`, `--health-check`, `--restart`, `--verify`, `--cpu-shares`, `--memory-max`, `--disk-iops`, `--disk-bps` | Create and start a unikernel VM |
| `uni build` | `--name`, `--tag`, `--pkg`, `--lang`, `--platform` | Build a unikernel image from binary or source directory |
| `uni images` | â€” | List local images |
| `uni rmi` | â€” | Remove a local image |
| `uni push` | â€” | Push image to registry |
| `uni pull` | `--verify` | Pull image from registry |
| `uni search <registry>/<query>` | â€” | Search remote registry repositories |
| `uni sign <image>` | `--key` | Sign a local image with Ed25519 key |
| `uni verify <image>` | â€” | Verify image signature |
| `uni ps` | â€” | List running VMs |
| `uni status` | â€” | Show VM summary with health/restart info |
| `uni logs <id>` | â€” | Show captured serial console output |
| `uni stop <id>` | `--force` | Stop (or kill) a VM |
| `uni rm <id>` | â€” | Remove a stopped VM |
| `uni stats <id>` | `--watch`, `--interval` | Live resource usage (CPU, memory, network I/O) |
| `uni inspect <id>` | â€” | Full VM detail as JSON |
| `uni exec <id> <cmd>` | â€” | Execute command in VM |
| `uni compose up/down/ps/logs` | `--volumes` | Multi-service orchestration |
| `uni volume create/ls/rm/inspect` | â€” | Manage persistent volumes |
| `uni network create/ls/inspect/rm` | `--subnet`, `--driver` | Manage networks |
| `uni dns resolve/list` | `--network` | Resolve and inspect internal VM DNS records |
| `uni node ls` | â€” | List cluster members with status + resource capacity |
| `uni run --network <name>` | `--network`, `--ip` | Auto-allocate IP from network |
| `uni kernel check/update/list/use` | â€” | Manage kernel tools |
| `uni pkg list/search/get/remove/create` | â€” | Manage packages |
| `uni cp <src> <dst>` | â€” | Copy files to/from VM |
| `uni upgrade` | â€” | Self-update CLI binary |

Build pipeline in `internal/vm/qemu.go::buildCmd`:
- Network priority: `NetworkName` (TAP) > `PortMaps` non-empty (SLIRP `hostfwd`) > `-net none`.
- SLIRP user-mode (`-netdev user,...,hostfwd=tcp::8080-:80`) does not need TAP/bridge or root, works on any platform â€” preferred for `-p`.
- Env vars are passed via `-fw_cfg name=opt/uni/env,string=KEY=VAL\nâ€¦`. The kernel reads this at boot.
- Network config (static IP) is passed via `-fw_cfg name=opt/uni/network,string=IP/MASK,GATEWAY`. Format uses `Config.SubnetMask` (not hardcoded `/24`): `10.0.0.2/24,10.0.0.1`.
- Bridge and TAP interfaces use dynamic names from the network store: bridge = `uni-br-<network-name>`, TAP remains as `Config.NetworkName`.
- Volumes attach as extra `-drive file=...,format=raw,if=virtio,index=N` after the boot disk (index 0).

## Kernel Patches (uni-specific additions to Nanos fork)

- **`kernel/src/drivers/fw_cfg.{c,h}`** â€” QEMU fw_cfg driver, x86-only (uses I/O ports `0x510`/`0x511`). Reads named files (e.g. `opt/uni/env`) by walking the directory at entry `0x0019`. Confirms `"QEMU"` signature before use; safe no-op on bare metal.
- **`kernel/src/unix/env_inject.c`** â€” `env_inject_from_fw_cfg(root)` reads `opt/uni/env` and merges entries into `root[environment]` tuple. Called from `stage3.c::startup()` before `exec_elf` builds the user stack envp. Compiles on aarch64 too (`#ifdef __x86_64__` guards the body to a stub).
- **`kernel/src/unix/net_inject.c`** â€” `net_inject_from_fw_cfg(root)` reads `opt/uni/network` and injects static IP configuration (`ipaddr`, `netmask`, `gateway`) into root tuple. `init_network_iface()` picks this up to configure the first ethernet interface instead of DHCP. x86-only (fw_cfg dependency).
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
- Patch bump (`0.1.0 â†’ 0.1.1`) for fixes; minor bump (`0.1.0 â†’ 0.2.0`) for features.
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
- Remote: `AitorConS/UniCli` (renamed). Pushes work but emit a redirect notice â€” not a hook failure.

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
| Network config fw_cfg | `internal/vm/qemu.go::buildNetworkCfgArgs` â€” uses `Config.SubnetMask` (not hardcoded `/24`); format: `IP/MASK,GW` |
| Host-side bridge/TAP | `internal/network/bridge_linux.go` â€” `CreateBridge`, `AttachTAP`, `DestroyBridge`; bridge name from `Config.BridgeName` (not hardcoded) |
| Network Store + IPAM | `internal/network/store.go` â€” `Store` with `Create/Get/List/Remove/AllocateIP/ReleaseIP`; persistent `~/.uni/networks/<name>/` with `meta.json` + `state.json`; subnet allocator from 10.100.0.0/16 |
| iptables port forwarding | `internal/network/portfwd_linux.go` â€” DNAT + MASQUERADE with `-i tapName` |
| Package index/store | `internal/package/package.go` â€” `Store`, `FetchIndex`, `Search`, `Extract`, `ExtractedFiles`, `RemoveAll`, `Create` |
| Package download with SHA-256 | `internal/package/package.go::Download` â€” verifies `Package.SHA256` after download, removes archive on mismatch; skips when empty |
| Package creation | `internal/package/package.go::Create` â€” creates local package archive from binary + optional libs, computes SHA256, writes meta.json |
| `uni pkg` commands | `cmd/uni/pkg.go` â€” list, search, get, remove (all versions), create (from binary + libs) |
| Package resolution (build) | `cmd/uni/build.go::resolvePackages` â€” download, extract, list files for manifest |
| Manifest with package files | `internal/image/builder.go::BuildManifest` â€” includes extracted package files as manifest children |
| `uni pkg` CLI tests | `cmd/uni/pkg_test.go` â€” search, get, list, remove, remove-all-versions, create, not-found, parsePkgRef |
| `resolvePackages` tests | `cmd/uni/resolve_test.go` â€” downloadâ†’extractâ†’list pipeline, specific version, not-found, multiple packages |
| Package pipeline integration test | `tests/integration/package_pipeline_test.go` â€” full Downloadâ†’Extractâ†’ExtractedFilesâ†’BuildManifest end-to-end |
| `uni cp` (to VM) | `cmd/uni/cp.go::cpToVM` â€” dump â†’ copy file â†’ mkfs rebuild |
| Compose shared volumes | `internal/compose/types.go::VolumeConfig` + `cmd/uni/compose.go::newComposeUpCmd` |
| CLI self-update | `cmd/uni/upgrade.go::replaceBinary` |
| CLI version (injected at build) | `cmd/uni/main.go::version` â€” set via `-X main.version` in `main.yml` |
| Image signing and verification | `internal/signing/signing.go` â€” Ed25519 key generation, signing, verification, key store (`~/.uni/keys/`) |
| `uni sign` / `uni verify` | `cmd/uni/sign.go` â€” sign local images, verify signatures; `--verify` flag on `uni run` and `uni pull` |
| Auto-generated self-signed TLS | `internal/autotls/autotls.go` â€” RSA 2048-bit key + X.509 cert, 365 days validity, stored at `~/.uni/registry/tls/`, reused on subsequent starts |
| Standalone registry | `cmd/unireg/main.go` â€” independently deployable registry server with same API/auth/TLS/GC as embedded daemon registry |
| Docker compatibility tests | `tests/integration/docker_compat_test.go` â€” validates Docker CLI patterns against registry server |
| Build driver framework | `internal/builder/builder.go` â€” `Driver` interface, `Lang` type, `DetectLanguage()`, `GoDriver` + `RustDriver` (full ELF builds), `NodeDriver` + `PythonDriver` (interpreted: SourceDir+Packages flow), `unikernel.toml` parser |
| Build ignore file | `internal/builder/unignore.go` â€” `.unignore` parser with `.gitignore`-style patterns, `DefaultIgnorePatterns`, `IgnoreMatcher.Match()`, used by `sourceFiles()` in build CLI |
| Build cache | `internal/builder/cache.go` â€” `BuildCache` with deterministic `CacheKey` hash from source files + lang + entrypoint, `Has`/`Store`/`Get` for skip-rebuild optimization |
| Platform types | `internal/builder/platform.go` â€” `Platform` type, `ParsePlatform`, `GoCrossCompileEnv`, `RustTarget`, `IsNative`, `--platform` flag on `uni build` |
| `unikernel.toml` parser | `internal/builder/config.go` â€” `Config`, `LoadConfig`, `validateConfig`, `LangHint()`; validates build.lang, run.memory, run.cpus, run.ports, env |
| Build CLI (`--lang`) | `cmd/uni/build.go` â€” `--lang go` flag, auto-detection for directory args, `unikernel.toml` loaded for lang/entrypoint/args, SourceDir+Packages flow for interpreted languages |
| Health check probes | `internal/vm/health.go` â€” `HealthChecker`, TCP/HTTP probes, backoff, `probeTarget` |
| Restart policy logic | `internal/vm/qemu.go::monitor` â€” evaluates `RestartConfig` on process exit, calls `restartVM` with backoff |
| Restart policy CLI flag | `cmd/uni/run.go::parseRestartPolicy` â€” `--restart never/on-failure/always[:N]` |
| Health check CLI flag | `cmd/uni/run.go::parseHealthCheck` â€” `--health-check tcp:PORT/http:PORT:/path` |
| VM persistence | `internal/vm/filestore.go` â€” `FileStore` with `state.json`, `Restore()` on daemon startup |
| VM status command | `cmd/uni/status.go` â€” `uni status` shows VM summary with health/restart info |
| VM stats command | `cmd/uni/stats.go` â€” `uni stats` shows live CPU/memory/network with `--watch` mode |
| Runtime stats collector | `internal/vm/stats.go` â€” `RuntimeStats`, `StatsCollector`, `ProcStatsCollector` (Linux), `NoopStatsCollector` (fallback); per-VM stats via `VM.SetStatsProvider` |
| Network CLI | `cmd/uni/network.go` â€” `uni network create/ls/inspect/rm`, `--subnet` and `--driver` flags |
| Network config auto-IP | `cmd/uni/run.go` â€” `--network <name>` resolves network from store, auto-allocates IP via IPAM |
| Compose network integration | `cmd/uni/compose.go` â€” creates networks in `compose up`, assigns IPs to services, removes in `compose down` |
| Compose health checks | `cmd/uni/compose.go` â€” `health_check:` field in compose services, mapped to `api.HealthCheckSpec`, wait-for-healthy in `compose up` |
| Compose restart policies | `cmd/uni/compose.go` â€” `restart:` field in compose services, mapped to `api.RestartSpec` |
| Compose YAML validation | `internal/compose/parser.go` â€” `validateHealthCheckSpec`, `validateRestartSpec` |
| Structured JSON logging | `internal/slogformat/handler.go` â€” `JSONHandler` implementing `slog.Handler`, outputs JSON lines with `ts`/`level`/`msg`/attributes |
| OpenTelemetry tracing | `internal/tracing/tracing.go` â€” `Provider` with OTLP gRPC export, no-op when `--trace-addr` empty; `internal/tracing/spans.go` â€” VM lifecycle span helpers, `RecordError`, `SpanWithRetryAttrs` |
| SQLite VM store | `internal/vm/sqlitestore.go` â€” `SQLiteStore` implementing `Store` interface, `--vm-store sqlite` flag on `unid` |
| File-to-SQLite migration | `internal/vm/migrate.go` â€” `Migrator` with idempotent `state.json â†’ sqlite` migration |
| Dashboard stats polling | `internal/ui/handler.go` â€” `/ui/api/vm/{id}/stats` JSON endpoint, 3s polling on VM detail page |
| Resource quotas (cgroup v2) | `internal/vm/cgroup.go` (Linux) / `internal/vm/cgroup_stub.go` (non-Linux) â€” `CgroupManager.Apply(pid, CgroupLimit)`, `Remove()`, `IsCgroupV2Available()` |
| Resource quotas CLI | `cmd/uni/run.go` â€” `--cpu-shares` (1â€“10000), `--memory-max` (e.g. 512M, 1G), `parseMemoryMax()` |
| Cluster membership (SWIM) | `internal/cluster/cluster.go` â€” `SwimCluster` with `Join`, `Start`, `Leave`, `HandleGossip`, `Members`, `MemberListerAdapter`; `RegisterGossipHandler` for `/cluster/gossip` HTTP endpoint |
| Cluster daemon flags | `cmd/unid/main.go` â€” `--cluster-addr` and `--join` flags, `clusterMemberAdapter` for API integration |
| `uni node ls` CLI | `cmd/uni/node.go` â€” `uni node ls` with table/JSON output |
| `Node.List` JSON-RPC | `internal/api/server.go::handleNodeList` â€” `Node.List` dispatch, `ClusterMemberLister` interface |

## Internal Packages

| Package | Description |
|---|---|
| `internal/api/` | JSON-RPC 2.0 server/client over Unix socket. VM lifecycle RPC methods. |
| `internal/compose/` | Compose YAML parser, validator, Kahn's topological sort with cycle detection, shared volumes. |
| `internal/image/` | Image build pipeline (ELF validation, mkfs, SHA256, `BuildManifest` with package files) + content-addressable store. |
| `internal/network/` | TAP device + Linux bridge setup, iptables port forwarding (Linux-only), **Network Store + IPAM** (`store.go`) with persistent `~/.uni/networks/<name>/` directories. Network type with subnet allocator (10.100.0.0/16 â†’ /24 blocks), AllocateIP/ReleaseIP, bridge-per-network convention (`uni-br-<name>`). |
| `internal/package/` | Package index fetch, local store, download (SHA-256 verified), extract (tar.gz), search, remove. |
| `internal/registry/` | Hybrid registry server/client with legacy `/v2/images` and OCI `/v2/...` flows, persistent OCI blobs/manifests, optional bearer/JWT auth, and optional registry TLS. |
| `internal/signing/` | Ed25519 image signing and verification. Key pair generation and storage at `~/.uni/keys/`. Signature files stored alongside manifests (`manifest.json.sig`). Verification policy: `off` (default), `warn` (log warnings), `enforce` (fail on missing/invalid). |
| `internal/autotls/` | Auto-generation of self-signed TLS certificates for the registry. Generates RSA 2048-bit key + X.509 cert valid 365 days, stored at `~/.uni/registry/tls/`. Reuses existing certs on subsequent starts. |
| `internal/builder/` | Build driver framework for multi-language `uni build`. `Driver` interface with `Detect`/`Build`/`Lang`, `GoDriver` + `RustDriver` (full ELF builds), `NodeDriver` + `PythonDriver` (interpreted: SourceDir+Packages flow), `DetectLanguage()` auto-detection from project markers, `unikernel.toml` config, `.unignore`, build cache, `--platform` cross-compilation. |
| `internal/scheduler/` | DNS resolver for name-to-IP lookups over running VMs (Phase 7.6). |
| `internal/tools/` | Kernel tools management: download, version check, platform-specific mkfs resolution. |
| `internal/vm/` | Core package: VM lifecycle state machine, QEMU wrapper, port map parser, VM registry store (`FileStore` default, `SQLiteStore` via `--vm-store sqlite`), `Migrator` for idempotent `state.json â†’ sqlite`, network cfg via fw_cfg, health checks, restart policies, persistence, runtime stats. |
| `internal/volume/` | Named volume management: sparse disk creation, attach/detach as virtio-blk devices. |
| `internal/cluster/` | SWIM-style gossip membership over HTTP. `SwimCluster` with ping/ack/suspicion/dead states, `--join` seed nodes, `RegisterGossipHandler` for `/cluster/gossip` endpoint, `MemberListerAdapter` for API integration. |
| `internal/metrics/` | Prometheus metrics collection for `unid`. `Collectors` with VM state gauges, lifecycle counters, registry push/pull counters, build info. `VMStateUpdater` polls VM Manager and updates gauges. `Serve()` starts HTTP `/metrics` and `/health`. |
| `internal/slogformat/` | Custom `slog.Handler` for structured JSON logging. `JSONHandler` outputs JSON lines with `ts`, `level`, `msg`, and arbitrary attributes. Wired via `--log-format text|json` flag on `unid`. |
| `internal/tracing/` | OpenTelemetry tracing for `unid`. `Provider` creates OTLP gRPC TracerProvider (no-op when `--trace-addr` is empty). Spans for VM lifecycle events (`vm.create`, `vm.start`, `vm.stop`, `vm.kill`, `vm.remove`, `vm.lifecycle`). `RecordError` and `SpanWithRetryAttrs` helpers. |
| `internal/ui/` | Web dashboard served on `--ui-addr`. Go-templated HTML listing VMs with state and health. VM detail page at `/ui/vm/{id}` with config, health, ports, env, log tail, live stats. JSON API at `/ui/api/vms`, `/ui/api/vm/{id}`, `/ui/api/vm/{id}/logs`, `/ui/api/vm/{id}/stats`. Dark theme, responsive layout, no JS framework. |

## Stub Packages (placeholders for future phases)

| Path | Phase | Purpose |
|---|---|---|
| `pkg/` | 6+ | Public shared libraries |
| `tests/unit/` | â€” | Empty; unit tests are co-located with source files |

## Session Handoff (2026-05-17)

### Completed This Session

- **Phase A â€” Cierre documental Phase 10:** Fixed roadmap GoDriver 9.1 checkbox (already implemented), fixed AGENTS.md E2E status (already enabled, was stale TODO), replaced nightly webhook TODO with configuration note.
- **Phase B â€” E2E Test Expansion:** Skipped (requires KVM/QEMU, can't verify on Windows).
- **Phase C â€” Coverage Gaps:** Raised 5 internal packages to 80%+ coverage. metrics: 14%â†’90%, tracing: 46%â†’80.5%, vm: 60%â†’80.9%, image: 62%â†’90.3%, registry: 64%â†’83.4%. Added 4500+ lines of test code across 18 new test files.
- **Phase D â€” Package Toolchain:** Added `Store.Create()` and `uni pkg create <name>[:<version>] <binary>` CLI command with `--libs`, `--description`, `--runtime` flags. Added `packages.yml` CI workflow for building official packages. Fixed `pkg.IndexURL` casing (UniCLi â†’ UniCli). Marked roadmap 6.6.1 as complete.

### Coverage Snapshot

| Package | Before | After |
|---|---|---|
| `internal/metrics` | 14.0% | 90.0% |
| `internal/tracing` | 46.3% | 80.5% |
| `internal/vm` | 59.9% | 80.9% |
| `internal/image` | 61.6% | 90.3% |
| `internal/registry` | 64.1% | 83.4% |

### Next Steps

1. **Phase E â€” Cloud Native (large scope):**
   - E.1: `internal/service/` package with Service abstraction (DesiredReplicas, Strategy)
   - E.2: `uni scale <name>=N` CLI command with `--watch` flag
   - E.3: Rolling updates (create new â†’ health check â†’ DNS update â†’ graceful stop old)
   - E.4: Multi-VM DNS round-robin in `internal/scheduler/resolver.go`
   - E.5: (Optional) Lightweight load balancer with iptables DNAT
2. **E2E Test Expansion** (when KVM runner available): image round-trip, compose networking, volume persistence, registry auth
3. **Remaining Phase D items** (require Linux): D.1 (node:v20 build), D.3 (--missing-files ldd analysis), D.6 (uni pkg push)

### Validation Commands

- `go test ./cmd/... ./internal/... -count=1`
- `golangci-lint run --timeout 5m ./...`
