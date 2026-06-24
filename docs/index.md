---
layout: home
title: Home
nav_order: 1
---

# Uni — Unikernel Engine
{: .fs-9 }

A Docker-like engine for building, running, and orchestrating unikernel virtual machines on KVM/QEMU.
{: .fs-6 .fw-300 }

[Get Started]({% link getting-started.md %}){: .btn .btn-primary .fs-5 .mb-4 .mb-md-0 .mr-2 }
[View on GitHub](https://github.com/AitorConS/jerboa){: .btn .fs-5 .mb-4 .mb-md-0 }

---

## What is a Unikernel?

A **unikernel** is a single-purpose virtual machine: your application code compiled together with only the OS components it needs, running as a minimal VM. No shell, no package manager, no unused services — just your app and a tiny kernel.

Compared to containers:

| | Container | Unikernel |
|---|---|---|
| Isolation | Shared kernel (namespaces) | Full VM hardware boundary |
| Attack surface | Large (host kernel exposed) | Tiny (minimal kernel) |
| Boot time | ~100ms | ~50ms |
| Memory overhead | ~10–50 MB | ~2–5 MB |
| Runtime | Any Linux binary | Static ELF only |

## What is Uni?

`jerboa` is a command-line tool (plus a background daemon `jerboad`) that manages the full unikernel lifecycle — the same way Docker manages containers.

```
jerboa build ./myapp                       # build a unikernel image from source or a binary
jerboa run hello:latest                    # start a unikernel VM (detached by default)
jerboa run hello:latest --attach           # start and stream serial console output
jerboa network create app
jerboa run hello:latest --network app -p 8080:80   # auto-allocated IP + port forwarding
jerboa dns list --network app              # inspect internal DNS records
jerboa ps                                  # list running VMs
jerboa logs <id>                           # read serial console output
jerboa stop <id>                           # graceful shutdown
jerboa cp <id>:/path/file.txt ./local.txt  # copy a file out of a stopped VM
jerboa compose up stack.yaml               # start a multi-service application
jerboa service run web app:latest --replicas 3 --network app  # scale a service to N replicas
jerboa kernel update                       # update the cached kernel tools
jerboa upgrade                             # self-update jerboa and jerboad
```

## Architecture Overview

```
┌──────────────────────────────────────────────────┐
│  jerboa CLI  (cobra commands)                       │
│  run · build · compose · service · network · ... │
└──────────────────────┬───────────────────────────┘
                       │  JSON-RPC over Unix socket
┌──────────────────────▼───────────────────────────┐
│  jerboad  (daemon)                                  │
│  ┌────────────┐  ┌─────────────────────────────┐ │
│  │ VM Manager │  │ Image Store                 │ │
│  │ QEMU/KVM   │  │ content-addressed (SHA256)  │ │
│  └────────────┘  └─────────────────────────────┘ │
│  Networking · Volumes · Compose · Services       │
│  Cluster gossip · Metrics · Tracing · Dashboard  │
└──────────────────────┬───────────────────────────┘
                       │
┌──────────────────────▼───────────────────────────┐
│  Nanos Kernel (C + ASM fork)                     │
│  Boots application images on KVM/QEMU            │
└──────────────────────────────────────────────────┘
```

See [Architecture]({% link architecture.md %}) for the full breakdown of every subsystem.

## Key Features

- **Build from source or binary** — `jerboa build` compiles Go, Node.js, Python, or Rust projects directly with built-in language drivers, or packages a pre-compiled static ELF binary; supports multi-stage builds via `unikernel.toml`
- **Content-addressed image store** — images are a JSON manifest + raw disk, addressed by SHA256 digest, with optional Ed25519 signing and verification (`jerboa sign` / `jerboa verify`)
- **Full VM isolation** — every service runs in its own KVM virtual machine, with optional cgroup v2 CPU/memory quotas and disk I/O throttling
- **Compose support** — define multi-service stacks in YAML with dependency ordering, health checks, restart policies, and replica scaling
- **Services** — `jerboa service` runs and manages groups of replica VMs behind a shared name, with rolling updates and scaling
- **Managed networking & internal DNS** — create isolated bridge networks with auto-allocated IPs, resolve services by name, and round-robin across replicas (`jerboa dns resolve-all`)
- **Persistent volumes** — named, reusable disk images that survive VM restarts (`jerboa volume`)
- **Cluster mode** — gossip-based multi-node membership for distributing VMs across hosts (`jerboa node ls`)
- **Built-in observability** — Prometheus metrics, OpenTelemetry tracing, structured JSON logs, live `jerboa stats`, and a read-only web dashboard (see [Observability]({% link observability.md %}))
- **Attach mode** — stream VM serial console output in real time with `--attach` (default is detached)
- **Graceful lifecycle** — SIGTERM → 30s grace period → SIGKILL, with configurable restart policies and health checks
- **JSON output** — every command supports `--output json` for scripting
- **Versioned releases** — the CLI and the kernel are independently versioned with semver; `jerboa upgrade` self-updates the binaries, `jerboa kernel update` updates the kernel tools
