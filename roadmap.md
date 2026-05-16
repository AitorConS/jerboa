# Unikernel Engine — Roadmap

> Stability first. Each phase must pass all tests + lint before the next begins. No exceptions.

---

## Current status: Phase 10 — Observability & Production Hardening (complete ✅)

### Phase 10 progress (2026-05-15)

- ✅ 10.1 — Prometheus metrics endpoint (`/metrics`, `/health`), `--metrics-addr` flag on `unid`
- ✅ 10.2 — OpenTelemetry trace export (`--trace-addr`), VM lifecycle spans
- ✅ 10.3 — Structured JSON logging (`--log-format text|json`)
- ✅ 10.4 — `uni stats <id>` — live CPU%, memory, network I/O per VM (with `--watch` mode)
- ✅ 10.5 — Web dashboard on `/ui`
  - ✅ 10.5.1 — Base dashboard: VM list with state and health, served on `--ui-addr`, JSON API at `/ui/api/vms`
  - ✅ 10.5.2 — VM detail page at `/ui/vm/{id}` with config, health, ports, env, log tail; JSON endpoints `/ui/api/vm/{id}` and `/ui/api/vm/{id}/logs`
  - ✅ 10.5.3 — Metrics polling: `/ui/api/vm/{id}/stats` JSON endpoint, live stats section with 3s polling on VM detail page
- ✅ 10.6 — Resource quotas (cgroup v2)
- ✅ 10.7 — I/O throttling
- ✅ 10.8 — Multi-node basic cluster
- ✅ 10.9 — `uni node ls`
- ✅ 10.10 — Daemon state persistence (SQLite-backed)
  - ✅ 10.10.1 — SQLiteStore implementation with `--vm-store sqlite` flag
  - ✅ 10.10.2 — Idempotent migration from state.json to SQLite
  - ✅ 10.10.3 — Daemon restart hardening: health status persisted and restored on daemon restart
- ✅ 10.11 — `govulncheck` + `trivy` in nightly CI; fail on HIGH/CRITICAL
- ✅ 10.12 — Documentation: observability guide, architecture dashboard section, repo URL fix
- CI baseline: Go `1.25` + `golangci-lint` `v2.12.2` (config `version: "2"`)

Phases 0–9 are complete. All core features (VM lifecycle, image system, CLI, compose, runtime, packages, orchestrator, registry, build system) are shipped.

---

## Phase 0 — Foundation & Kernel Fork `Weeks 1–3`

**Goal:** reproducible kernel build, boots hello-world ELF on QEMU.

### Steps

- [x] 0.1 — Fork Nanos repo into `kernel/`, strip vendor/cloud-specific bits (AWS, HyperV, VMware, Xen, riscv64)
- [x] 0.2 — Set up cross-compiler toolchain (x86_64-elf-gcc, nasm) — runs in CI, verify locally on Linux runner
- [x] 0.3 — Write `Makefile` targets: `kernel`, `clean`, `test-kernel`
- [x] 0.4 — Verify kernel boots on `qemu-system-x86_64` (KVM mode) — needs CI green first
- [x] 0.5 — Boot a static hello-world ELF binary end-to-end via QEMU — `tests/e2e/phase0_boot_test.go`
- [x] 0.6 — Document kernel/motor interface: image format, boot params → `kernel/INTERFACE.md`
- [x] 0.7 — Add C test suite under `kernel/test/` (full Nanos unit suite imported)
- [x] 0.8 — CI: `make kernel` passes in GitHub Actions (`ubuntu-latest`) — pending first push + CI run

**Done when:** any developer can clone + run `make kernel && make test-kernel` and get a passing build. QEMU boots ELF. CI green.

---

## Phase 1 — VM Manager (unid core) `Weeks 4–6`

**Goal:** `uni run ./hello` works end-to-end.

### Steps

- [x] 1.1 — Go module init (`go mod init`), set up `cmd/uni`, `cmd/unid` entrypoints
- [x] 1.2 — Define `VMManager` interface in `internal/vm/vm.go`
- [x] 1.3 — Implement QEMU process wrapper (spawn, kill, monitor)
- [x] 1.4 — VM state machine: `created → starting → running → stopping → stopped`
  - All transitions logged with `slog`
  - `sync.RWMutex` for concurrent access
- [x] 1.5 — TAP device + Linux bridge setup (`internal/network/tap.go`)
- [x] 1.6 — Unix socket API: `unid` listens, `uni` connects (JSON-RPC)
- [x] 1.7 — `uni run <binary>` command (cobra) → delegates to `unid` via socket
- [x] 1.8 — Unit tests: VM state machine, socket protocol parsing
- [x] 1.9 — Integration test: spin up VM, assert it started, tear down
- [x] 1.10 — `make build` produces `uni` + `unid` binaries

**Done when:** `uni run ./hello` works. Unit + integration tests green. CI passes.

---

## Phase 2 — Image System `Weeks 7–9`

**Goal:** build/push/pull unikernel images round-trip.

### Steps

- [x] 2.1 — Define image manifest format (JSON, versioned) in `internal/image/manifest.go`
- [x] 2.2 — Image build pipeline: ELF binary → disk image + manifest
- [x] 2.3 — Content-addressable local store (SHA256 keyed)
- [x] 2.4 — `uni build`, `uni images`, `uni rmi` commands
- [x] 2.5 — Registry server (`internal/registry/`): HTTP, OCI-inspired API
- [x] 2.6 — `uni push` / `uni pull` client
- [x] 2.7 — Table-driven tests for manifest parser (valid/invalid/missing-fields)
- [x] 2.8 — Integration test: build → push → pull → run round-trip

**Done when:** full image round-trip works. Image store tested. Registry server tested. 80%+ coverage on `internal/image/`.

---

## Phase 3 — Full CLI `Weeks 10–11`

**Goal:** complete operational CLI with JSON output.

### Steps

- [x] 3.1 — `uni ps` — list running instances with metadata
- [x] 3.2 — `uni logs` — stream stdout from VM serial console
- [x] 3.3 — `uni stop` — graceful shutdown (SIGTERM → 30s timeout → kill)
- [x] 3.4 — `uni rm` — remove stopped instance + cleanup
- [x] 3.5 — `uni inspect` — detailed instance info (JSON)
- [x] 3.6 — `uni exec` — send signals to running instance
- [x] 3.7 — `--output json` flag on all commands
- [x] 3.8 — Errors to stderr, output to stdout (enforced in tests)
- [x] 3.9 — 81% unit coverage on `cmd/uni/`

**Done when:** all commands work. JSON output works. Coverage met. CI green.

---

## Phase 4 — Compose & Multi-service `Weeks 12–14`

**Goal:** `uni compose up` starts 2+ services on a virtual network.

### Steps

- [x] 4.1 — Define compose YAML format (services, networks, volumes)
- [x] 4.2 — YAML parser + validator (`internal/compose/`)
- [x] 4.3 — Dependency graph: topological sort for startup ordering (Kahn's algorithm)
- [x] 4.4 — Internal virtual network between compose services (network refs in YAML)
- [x] 4.5 — Shared volumes (virtio-blk backed)
- [x] 4.6 — `uni compose up / down / logs / ps`
- [x] 4.7 — E2E test: 2-service compose, services communicate via network

**Done when:** compose up with 2+ services. Inter-service networking works. E2E green.

---

## Phase 5 — Complete Runtime: Ports, Env, Volumes `Weeks 15–17`

**Goal:** `uni run` reaches feature parity with `docker run` for the common 80% of workloads.

The foundation is there (memory, CPUs), but no app that actually listens on a port or reads config
from the environment can be used today. This phase closes that gap before the package system lands,
because packages are useless without a working runtime model.

### 5.1 — Port Mapping

- [x] 5.1.1 — Add `-p / --port host:guest` flag to `uni run` (repeatable, e.g. `-p 8080:80 -p 443:443`)
- [x] 5.1.2 — Implement port forwarding in QEMU wrapper using SLIRP user-mode networking (`-netdev user,hostfwd=...`) as fast path
- [x] 5.1.3 — TAP/bridge path: add iptables DNAT rules via `internal/network/portfwd.go` (Linux only)
- [x] 5.1.4 — Port map stored in VM config, visible in `uni inspect` and `uni ps --ports`
- [x] 5.1.5 — Expose ports in compose YAML (`ports: ["8080:80"]`) mirroring Docker Compose syntax
- [x] 5.1.6 — Unit tests: port spec parser (ranges, UDP, edge cases)

### 5.2 — Environment Variable Injection

- [x] 5.2.1 — Add `-e / --env KEY=VALUE` flag to `uni run` (repeatable)
- [x] 5.2.2 — Add `--env-file <path>` flag: read `KEY=VALUE` lines from file, identical to Docker
- [x] 5.2.3 — Wire env vars through the API call → QEMU fw_cfg → kernel reads `opt/uni/env`
- [x] 5.2.4 — Kernel patch in `kernel/src/drivers/fw_cfg.c` + `kernel/src/unix/env_inject.c`:
  reads `opt/uni/env` from QEMU fw_cfg at boot (I/O ports 0x510/0x511), parses `KEY=VAL\n…`,
  merges into `root[environment]` before `exec_elf` builds the user stack envp.
  Verified end-to-end: `uni run webenv:latest -e MESSAGE=hello -p 4333:4333` → `os.Getenv("MESSAGE") == "hello"`.
- [x] 5.2.5 — Env vars in compose YAML (`environment:`) fully functional

### 5.3 — Volume Mounts & Persistent Storage

- [x] 5.3.1 — `internal/volume/` package: create, attach, detach raw virtio-blk disk images
- [x] 5.3.2 — `-v / --volume name:guestpath` flag on `uni run`; named volumes live in `~/.uni/volumes/`
- [x] 5.3.3 — `uni volume create/ls/rm/inspect` subcommands
- [x] 5.3.4 — Volume lifecycle: volumes persist across VM restarts
- [x] 5.3.5 — Read-only mounts: `-v name:guestpath:ro`
- [x] 5.3.6 — Shared volumes between compose services (same volume name in multiple services)
- [x] 5.3.7 — Integration test: write file in VM, stop, restart, data survives

### 5.4 — Named Instances & UX Polish

- [x] 5.4.1 — `--name <id>` flag on `uni run`; visible in `uni inspect`
- [x] 5.4.2 — `-d / --detach` flag (default) vs `--attach` (stream serial output to terminal)
- [x] 5.4.3 — `uni run --rm` auto-remove instance on exit
- [x] 5.4.4 — Static IP assignment: `--ip <addr>` flag (requires TAP networking)
- [x] 5.4.5 — `uni cp <id>:<guestpath> <localpath>` — copy files to/from a stopped VM (requires dump tool)

**Done when:** `--attach`, `--ip`, `uni cp` implemented. Volume integration test green. TAP/bridge DNAT optional.

---

## Phase 6 — Package System `Weeks 18–21` ✅ complete

**Goal:** `uni pkg load node:v20 app.js -p 3000:3000` runs a Node.js app with zero manual compilation.

This is the single biggest usability gap. Right now every app must be a static ELF binary — no
interpreted languages, no dynamic-linked apps, no standard runtimes. The package system provides
pre-compiled, versioned runtime packages (Node.js, Python, Redis, Nginx, …) that can be combined
with user code to produce a ready-to-run unikernel image in one command.

### 6.1 — Package Format & Local Cache

- [x] 6.1.1 — Package index + metadata model implemented in `internal/package/`
- [x] 6.1.2 — Local package cache at `~/.uni/packages/<name>/<version>/` (`files.tar.gz`, `files/`, `meta.json`)
- [x] 6.1.3 — `internal/package` store: install/list/remove/lookup by name:version
- [x] 6.1.4 — SHA256 verification on download when checksum is provided

### 6.2 — `uni pkg` CLI

- [x] 6.2.1 — `uni pkg list`
- [x] 6.2.2 — `uni pkg search <query>`
- [x] 6.2.3 — `uni pkg get <name:version>`
- [x] 6.2.4 — `uni pkg remove <name:version>` / remove all versions by name

### 6.3 — Build integration with `uni build --pkg`

- [x] 6.3.1 — `uni build --pkg <name[:version]>` downloads and resolves package files
- [x] 6.3.2 — Package files are included in image manifest (`BuildManifest`)
- [x] 6.3.3 — End-to-end package pipeline tests (download → extract → manifest)

### 6.4 — Package Index

- [x] 6.4.1 — JSON index client in `internal/package` with configurable `IndexURL` (test-overridable var)
- [x] 6.4.2 — Package metadata consumed from index + archive URLs
- [ ] 6.4.3 — Self-hosted index server tooling (deferred)

### 6.5 — Official Package Library (first wave)

Build and publish these packages to the official index. Deferred to a dedicated distribution track.

**Language runtimes:**
- [ ] 6.5.1 — `node:v20` — Node.js 20 LTS (most common web backend runtime)
- [ ] 6.5.2 — `node:v22` — Node.js 22 LTS
- [ ] 6.5.3 — `python:3.12` — CPython 3.12 (static build with stdlib)
- [ ] 6.5.4 — `python:3.11` — CPython 3.11 (LTS for compatibility)
- [ ] 6.5.5 — `ruby:3.2` — MRI Ruby 3.2
- [ ] 6.5.6 — `lua:5.4` — Lua 5.4 (lightweight scripting)
- [ ] 6.5.7 — `php:8.3` — PHP 8.3 CLI

**Web servers:**
- [ ] 6.5.8 — `nginx:1.24` — Nginx static file server + reverse proxy
- [ ] 6.5.9 — `caddy:2.7` — Caddy with automatic HTTPS

**Databases & data stores:**
- [ ] 6.5.10 — `redis:7.2` — Redis in-memory data store
- [ ] 6.5.11 — `sqlite:3.45` — SQLite CLI + library

**Tools:**
- [ ] 6.5.12 — `curl:8.6` — cURL for inter-VM HTTP calls
- [ ] 6.5.13 — `jq:1.7` — JSON processor

### 6.6 — Package Creation Toolchain

- [ ] 6.6.1 — `uni pkg create <name> <binary> [--libs <lib>...]` — scaffold a new package from a local binary
- [ ] 6.6.2 — `uni pkg from-docker <image> --file <binary>` — convert a Docker image into a uni package (extract binary + libs)
- [ ] 6.6.3 — `--missing-files` flag on `uni pkg load`: detect and report missing dynamic libs at build time (uses `ldd` output analysis)
- [ ] 6.6.4 — `uni pkg push <name:version>` — push a locally created package to the index (requires `uni login`)
- [ ] 6.6.5 — CI pipeline for building official packages: cross-compile on GitHub Actions, publish to index on tag

**Done when:** package download/search/get/remove works, package files can be injected into built images with `--pkg`, and pipeline tests are green.

---

## Phase 7 — Orchestrator `Weeks 22–25` ✅ complete (7.0–7.7)

**Goal:** self-healing, scalable service management.

### Steps

- [x] 7.1 — Health check probes: TCP + HTTP, configurable interval/threshold
  - Compose syntax: `healthcheck: {test: ["HTTP", "http://localhost:8080/health"], interval: 10s, retries: 3}`
- [x] 7.2 — Restart policy: `on-failure`, `always` with exponential backoff
- [x] 7.3 — Auto-restart on crash: daemon monitors VM exit code, re-applies restart policy
- [ ] 7.4 — Rolling updates: drain old → start new → verify healthy → repeat; zero downtime (deferred)
- [ ] 7.5 — `uni scale <name>=N` — spawn or kill instances to reach target count (deferred)
- [x] 7.6 — Internal DNS resolver in `unid`: service name/IP lookup for running VMs, scoped names (`name.network`), ambiguity detection, CLI `uni dns`
- [x] 7.7 — `uni status` — VM summary view with health/restart info
- [x] 7.8 — Compose integration with health checks + restart directives + wait-for-healthy
- [x] 7.9 — Network Store + IPAM + `uni network` + compose network lifecycle

**Done when:** health checks, restart, status, DNS, network/IPAM, and compose integration are stable and fully tested. (Scale + rolling updates move to a future orchestrator expansion.)

---

## Phase 8 — Registry & Distribution `Weeks 26–28`

**Goal:** production-grade, OCI-compatible registry with auth.

**Architecture target:** the registry must end as an independently deployable service (separate process/deployment from `unid`), with the CLI interacting over network APIs.

### Steps

- [x] 8.0 — Pre-flight hardening: cross-platform TAP stubs + registry/tools failure-path test expansion

- [ ] 8.1 — OCI Distribution Spec v1.1 compatible API (push/pull/list/delete, manifests + blobs)
  - [x] 8.1.0 — OCI manifest foundational types and validation (`internal/ociregistry/types.go`)
  - [x] 8.1.1 — Content-addressable blob store foundation (`internal/ociblob/store.go`)
  - [x] 8.1.2 — Initial OCI registry endpoints (`/v2/_catalog`, blob uploads, manifest put/get/delete) in `internal/registry/server.go`
  - [x] 8.1.3 — Initial OCI client push/pull flow using blob + manifest APIs in `internal/registry/client.go`
  - [x] 8.1.4 — Persistent OCI manifest refs/store on disk (`internal/registry/ocistore.go`)
  - [x] 8.1.5 — `uni push/pull` prefer OCI flow with legacy fallback (`cmd/uni/push.go`)
  - [x] 8.1.6 — Add OCI `HEAD` support for blobs/manifests with `Docker-Content-Digest` headers
- [x] 8.2 — Image signing with Ed25519 keypair; `uni sign <image>` and `uni verify <image>`; key store at `~/.uni/keys/`; signatures stored alongside manifests (`manifest.json.sig`)
- [x] 8.3 — Signature verification on `uni pull` and `uni run` (`--verify off|warn|enforce`); warn logs missing/invalid signatures, enforce fails
- [x] 8.4 — Auth: token-based (JWT, scoped to repo + action); `uni login <registry>` stores credentials
  - [x] 8.4.0 — Optional static bearer auth gate in registry server (`--registry-token` / `UNI_REGISTRY_TOKEN`) with `WWW-Authenticate` challenge
  - [x] 8.4.1 — Optional JWT auth gate in registry server (`--registry-jwt-secret` / `UNI_REGISTRY_JWT_SECRET`) with repo/action scope enforcement
  - [x] 8.4.2 — Optional JWT issuer/audience validation (`--registry-jwt-issuer`, `--registry-jwt-audience`) with integration coverage
- [x] 8.5 — TLS: registry server generates self-signed cert on first boot; supports custom cert via config
  - [x] 8.5.0 — Support custom TLS cert/key config for registry HTTPS (`--registry-tls-cert`, `--registry-tls-key`)
  - [x] 8.5.1 — Auto-generate self-signed cert at `~/.uni/registry/tls/` when registry is enabled without custom TLS
- [x] 8.6 — Layer deduplication: blob-level dedup using content-addressable SHA256 (no duplicate blobs)
- [x] 8.7 — Garbage collection: `unid gc` removes blobs not referenced by any manifest
  - [x] 8.7.0 — Added `unid gc` command backed by manifest reference analysis and safe unreferenced blob deletion
- [x] 8.8 — `uni push / pull` work with auth headers and TLS
  - [x] 8.8.0 — Added global CLI registry auth/TLS options (`--registry-token`, `--registry-ca-cert`, `--registry-insecure`) with env var support
  - [x] 8.8.1 — Added CLI integration coverage for auth+TLS `uni push`/`uni pull`/`uni search` flows
- [x] 8.9 — `uni search <registry>/<query>` — search images on remote registry
  - [x] 8.9.0 — Added `uni search <registry>/<query>` using OCI catalog with substring filtering
- [x] 8.10 — Docker CLI compatibility: `docker push <registry>/<img>` works against a uni registry
  - [x] 8.10.0 — Added OCI route parsing support for nested repository names (`namespace/repo`) in blobs/manifests endpoints
  - [x] 8.10.1 — Added Docker-style `WWW-Authenticate` challenge format with `service` and repo/action `scope`
  - [x] 8.10.2 — Added OCI chunked blob upload support (`PATCH /blobs/uploads/<uuid>` then `PUT ...?digest=`) for Docker push compatibility
- [x] 8.11 — Registry service split: extract registry runtime from `unid` into an independently deployable service (`unireg`) with backward-compatible API behavior for `uni push/pull`

**Done when:** OCI-compatible push/pull with auth + signing + TLS bootstrap works. Docker CLI can push to the registry. `unireg` is independently deployable. Docker compatibility validated with integration tests.

---

## Phase 9 — Build System `Weeks 29–31`

**Goal:** `uni build` handles real multi-language projects, not just pre-compiled ELF binaries.

Today `uni build` requires a pre-compiled static ELF. This phase adds language-aware build pipelines
so developers can point at a project directory and get a runnable image.

### Steps

- [x] 9.0 — Build Driver framework: `internal/builder/` package with `Driver` interface, `Lang` type,
  `DetectLanguage()`, `GetDriver()`, and `AvailableDrivers()`. GoDriver as first implementation.
- [x] 9.0.1 — `uni build --lang go .` CLI flag wired to builder pipeline with auto-detection
- [x] 9.0.2 — Node/Python/Rust driver stubs with Detect() and "not yet implemented" Build()
- [x] 9.0.3 — Auto-detection for all four languages (go.mod, package.json, pyproject.toml/requirements.txt, Cargo.toml) + ambiguity detection
- [ ] 9.1 — `uni build --lang go .` — detect Go project (`go.mod`), build static binary (`CGO_ENABLED=0`), produce image
- [x] 9.2 — `uni build --lang node .` — NodeDriver: detect package.json, npm install, read engines.node, entrypoint from package.json
- [x] 9.3 — `uni build --lang python .` — PythonDriver: detect pyproject.toml/requirements.txt, pip install, read requires-python, entrypoint from scripts
- [x] 9.4 — `uni build --lang rust .` — RustDriver: detect Cargo.toml, cargo build --release --target x86_64-unknown-linux-musl
- [x] 9.5 — Auto-detect language if `--lang` omitted (inspect project files, fail loudly if ambiguous)
- [x] 9.6 — `unikernel.toml` config file parser and validator: `[build]` lang/entrypoint/args, `[run]` memory/cpus/ports, `[env]`
- [x] 9.7 — `uni build` reads `unikernel.toml` for build.lang, build.entrypoint, build.args
- [ ] 9.8 — Multi-stage builds: separate build environment from runtime image (reduce image size)
- [x] 9.9 — `.unignore` file: exclude files from the disk image (like `.dockerignore`)
- [x] 9.10 — Build cache: skip rebuild if source hash unchanged
- [x] 9.11 — `uni build --platform linux/amd64,linux/arm64` — multi-arch image output (amd64 + ARM)

**Done when:** Go, Node.js, Python, Rust projects each build and run end-to-end from source with a single `uni build` command.

---

## Phase 10 — Observability & Production Hardening `Weeks 32–36`

**Goal:** production-ready metrics, tracing, multi-node, and a web dashboard.

### Steps

- [x] 10.1 — Prometheus metrics endpoint in `unid` (`/metrics`): VM count, state transitions, CPU/memory per VM, port forwarding stats
- [x] 10.2 — OpenTelemetry trace export from `unid`: span per VM lifecycle event, exportable to Jaeger/Tempo
- [x] 10.3 — Structured log export: daemon aggregates all VM serial console output, exports as JSON lines (ship to Loki/Splunk/stdout)
- [x] 10.4 — `uni stats <id>` — live CPU%, memory usage, network I/O per VM (with `--watch` mode, `--interval` flag)
- [x] 10.5 — Web dashboard (Go-served, no JS framework): `/ui` on daemon port
  - [x] 10.5.1 — Base dashboard: VM list with state and health, served on `--ui-addr`
  - [x] 10.5.2 — VM detail page at `/ui/vm/{id}` with config, health, ports, env, log tail; JSON endpoints `/ui/api/vm/{id}` and `/ui/api/vm/{id}/logs`
  - [x] 10.5.3 — Metrics polling: `/ui/api/vm/{id}/stats` JSON endpoint, live stats section with 3s polling on VM detail page
- [x] 10.6 — Resource quotas per VM: cgroup v2 integration for CPU shares + memory hard limit (enforced at kernel level, not just QEMU hint)
- [x] 10.7 — I/O throttling: `--disk-iops` and `--disk-bps` limits via QEMU drive throttle
- [x] 10.8 — Multi-node basic cluster: `unid --join <peer>` — SWIM-style gossip membership over HTTP, `--cluster-addr` flag, `/cluster/gossip` HTTP endpoint
- [x] 10.9 — `uni node ls` — list cluster members with status + resource capacity
- [x] 10.10 — Daemon state persistence: SQLite-backed VM store; all VMs survive `unid` restart
- [x] 10.11 — `govulncheck` + `trivy` scan in nightly CI; block release on HIGH/CRITICAL CVEs
- [x] 10.12 — Documentation: observability guide, architecture dashboard section, repo URL fix

**Done when:** Prometheus scrapes metrics. Dashboard shows live instances. Daemon survives restart. Cluster membership works.

---

## Principles (enforced across all phases)

- Phase not done if: tests skipped, lint fails, happy-path only
- Every PR to `main` requires: lint + unit tests + kernel build + integration tests green
- Interfaces before implementations
- No global mutable state
- Functions under 50 lines
- All errors wrapped: `fmt.Errorf("context: %w", err)`
- Package first-wave binaries cross-compiled in CI — never hand-compiled locally

---

## Feature matrix

| Feature | Phase | Status |
|---|---|---|
| Port mapping (`-p host:guest`) | 5 | ✅ done (SLIRP) |
| Environment variables (`-e KEY=VAL`) | 5 | ✅ done |
| `--env-file` | 5 | ✅ done |
| Volume mounts (`-v name:path`) | 5 | ✅ done |
| Read-only volumes (`:ro`) | 5 | ✅ done |
| Named instances (`--name`) | 5 | ✅ done |
| Auto-remove (`--rm`) | 5 | ✅ done |
| Attach mode (`--attach`) | 5 | ✅ done |
| Static IP (`--ip`) | 5 | ✅ done |
| `uni cp` | 5 | ✅ done (from stopped VMs) |
| TAP/bridge iptables DNAT | 5 | ✅ done (Linux) |
| Volume integration test | 5 | ✅ done |
| Package system (`uni pkg list/search/get/remove`) | 6 | ✅ done |
| Node.js runtime package | 6 | ⬜ |
| Python runtime package | 6 | ⬜ |
| Redis / Nginx packages | 6 | ⬜ |
| Health checks + restart policies | 7 | ✅ done |
| Auto-scaling (`uni scale`) | 7 | ⬜ deferred |
| Internal DNS | 7 | ✅ done |
| OCI-compatible registry | 8 | ✅ done |
| Image signing | 8 | ✅ done |
| Registry auth (JWT) | 8 | ✅ done |
| Self-signed TLS bootstrap | 8 | ✅ done |
| Standalone registry (`unireg`) | 8 | ✅ done |
| Multi-language `uni build` | 9 | ✅ done |
| `unikernel.toml` project config | 9 | ✅ done |
| Build cache + `.unignore` | 9 | ✅ done |
| `--platform` cross-compilation | 9 | ✅ done |
| Prometheus metrics | 10 | ✅ done |
| Structured JSON logging | 10 | ✅ done |
| OpenTelemetry tracing | 10 | ✅ done |
| `uni stats` live metrics | 10 | ✅ done |
| Web dashboard | 10 | ✅ done |
| Multi-node cluster | 10 | ✅ done (SWIM gossip membership, uni node ls) |
| Daemon state persistence | 10 | ✅ done (FileStore + SQLiteStore + migration + restart hardening) |
