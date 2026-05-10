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
[View on GitHub](https://github.com/AitorConS/UniCli){: .btn .fs-5 .mb-4 .mb-md-0 }

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

`uni` is a command-line tool (plus a background daemon `unid`) that manages the full unikernel lifecycle — the same way Docker manages containers.

```
uni build ./myapp          # package ELF binary into an image
uni run hello:latest       # start a unikernel VM (detached by default)
uni run hello:latest --attach  # start and stream serial output
uni network create app
uni run hello:latest --network app                      # auto-allocated IP from IPAM
uni dns list --network app                              # inspect DNS records
uni ps                     # list running VMs
uni logs <id>              # read serial console output
uni stop <id>              # graceful shutdown
uni cp <id>:/path/file.txt ./local.txt  # copy file from stopped VM
uni compose up stack.yaml  # start a multi-service application
uni kernel update          # update the cached kernel tools
uni upgrade                # self-update uni and unid
```

## Architecture Overview

```
┌─────────────────────────────────────────────┐
│  uni CLI  (cobra commands)                  │
│  run · ps · logs · stop · build · compose   │
└──────────────────┬──────────────────────────┘
                   │  JSON-RPC over Unix socket
┌──────────────────▼──────────────────────────┐
│  unid  (daemon)                             │
│  ┌────────────┐  ┌────────────────────────┐ │
│  │ VM Manager │  │ Image Registry (HTTP)  │ │
│  │ QEMU/KVM   │  │ SHA256 content store   │ │
│  └────────────┘  └────────────────────────┘ │
└──────────────────┬──────────────────────────┘
                   │
┌──────────────────▼──────────────────────────┐
│  Nanos Kernel (C + ASM fork)                │
│  Boots static ELF binaries on KVM/QEMU      │
└─────────────────────────────────────────────┘
```

## Key Features

- **Build once, run anywhere** — image format is a JSON manifest + raw disk, content-addressed by SHA256
- **Full VM isolation** — every service runs in its own KVM virtual machine
- **Compose support** — define multi-service stacks in YAML with dependency ordering
- **Internal DNS** — resolve running services by name (`uni dns resolve web --network app`)
- **Registry** — hybrid legacy + OCI v2 registry; `uni push/pull` prefer OCI with automatic legacy fallback
- **Attach mode** — stream VM serial console output in real-time with `--attach` (default is detached with `-d`)
- **Static IP assignment** — assign a static IP to VMs when using TAP networking with `--ip`
- **TAP/bridge DNAT** — port forwarding works with TAP interfaces via iptables rules (Linux only)
- **File copy from VMs** — extract files from stopped VM disk images with `uni cp`
- **Graceful lifecycle** — SIGTERM → 30s grace period → SIGKILL
- **JSON output** — every command supports `--output json` for scripting
- **Versioned releases** — both the CLI and the kernel are independently versioned with semver; `uni upgrade` self-updates the binaries, `uni kernel update` updates the kernel tools
