---
layout: default
title: Compose
nav_order: 4
---

# Compose
{: .no_toc }

Compose lets you define and run multi-service unikernel applications from a single YAML file.

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

## File Format

```yaml
version: "1"

services:
  <service-name>:
    image: <name:tag or file path>
    memory: <256M|1G|...>
    cpus: <number>
    depends_on:
      - <other-service>
    networks:
      - <network-name>
    environment:
      - KEY=VALUE
    ports:
      - "host:guest[/tcp|udp]"
    volumes:
      - "name:guestpath[:ro]"

networks:
  <network-name>:
    driver: bridge

volumes:
  <volume-name>:
    size: <512M|1G|...>
```

---

## Fields Reference

### Top-level

| Field | Required | Description |
|---|---|---|
| `version` | Yes | Must be `"1"` |
| `services` | Yes | Map of service definitions (at least one) |
| `networks` | No | Map of network definitions |
| `volumes` | No | Map of volume definitions (auto-created on `compose up`) |

---

### Service fields

| Field | Required | Default | Description |
|---|---|---|---|
| `image` | Yes | — | Image `name:tag` from local store, or a file path to a bootable disk image (`.img`) built with `uni build` |
| `memory` | No | `256M` | VM memory (QEMU format: `256M`, `1G`, `4G`) |
| `cpus` | No | `1` | Number of virtual CPUs |
| `depends_on` | No | `[]` | Services that must start before this one |
| `networks` | No | `[]` | Logical networks to attach to |
| `environment` | No | `[]` | Environment variables as `KEY=VALUE` strings |
| `ports` | No | `[]` | Port mappings: `"host:guest"` or `"host:guest/udp"` |
| `volumes` | No | `[]` | Volume mounts: `"name:guestpath"` or `"name:guestpath:ro"` |

---

### Network fields

| Field | Required | Default | Description |
|---|---|---|---|
| `driver` | No | `bridge` | Network driver. Only `bridge` is supported |

### Volume fields

| Field | Required | Default | Description |
|---|---|---|---|
| `size` | No | `1G` | Volume size (QEMU format: `512M`, `1G`, `2G`) |

---

## Full Example

A web application with a frontend, a backend API, and a database:

```yaml
version: "1"

services:
  db:
    image: redis:latest
    memory: 512M
    cpus: 1
    networks:
      - backend-net
    volumes:
      - dbdata:/data

  api:
    image: myapi:v1.0
    memory: 256M
    cpus: 2
    depends_on:
      - db
    networks:
      - backend-net
      - frontend-net
    environment:
      - DB_HOST=db
      - DB_PORT=6379
      - LOG_LEVEL=info
    ports:
      - "8080:8080"

  web:
    image: myweb:v1.0
    memory: 128M
    cpus: 1
    depends_on:
      - api
    networks:
      - frontend-net
    environment:
      - API_URL=http://api:8080
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - staticfiles:/var/www/html:ro

networks:
  backend-net:
    driver: bridge
  frontend-net:
    driver: bridge

volumes:
  dbdata:
    size: 1G
  staticfiles:
    size: 512M
```

{: .note }
Volumes referenced in `volumes:` are **auto-created** on `compose up` if they do not already exist. To remove them, use `compose down --volumes`. Volumes that already exist on disk are left as-is and reused.

**Startup order** (resolved by dependency graph):

```
db  →  api  →  web
```

**Shutdown order** (always reversed):

```
web  →  api  →  db
```

---

## How Dependency Ordering Works

Uni uses **Kahn's topological sort** algorithm to determine startup order:

1. Build a dependency graph from all `depends_on` declarations
2. Start services with no dependencies first
3. When a service finishes starting, unlock any services that depended on it

If a **dependency cycle** is detected (e.g. A depends on B, B depends on A), `uni compose up` will fail immediately:

```
Error: compose up: compose: dependency cycle detected
```

---

## State File

When you run `uni compose up stack.yaml`, a state file is created in the same directory:

```
stack.yaml
.uni-compose-state.json   ← automatically created
```

Content:
```json
{
  "project": "myproject",
  "services": {
    "db":  "a3f8c2d1-7b4e-4a1f-8c2d-1a2b3c4d5e6f",
    "api": "b4e9d3e2-8c5f-5b2g-9d3e-2b3c4d5e6f7a",
    "web": "c5f0e4f3-9d6a-6c3h-ae4f-3c4d5e6f7a8b"
  },
  "service_networks": {
    "db": "backend-net",
    "api": "backend-net",
    "web": "frontend-net"
  },
  "service_ips": {
    "db": "10.100.0.2",
    "api": "10.100.0.3",
    "web": "10.100.1.2"
  },
  "created_volumes": [
    "dbdata",
    "staticfiles"
  ]
}
```

Commands `down`, `ps`, and `logs` read this file to know which VM IDs belong to the stack. `service_networks` and `service_ips` are used during `compose down` to deterministically release allocated IPs back to IPAM.

{: .warning }
Do not delete `.uni-compose-state.json` manually while the stack is running. If it gets lost, use `uni ps` to find the VM IDs and stop them individually with `uni stop`.

---

## Minimal Example

The simplest possible compose file — one service, no networks.

Build the image first, then reference it by name:

```bash
uni build ./hello-linux --name hello
```

```yaml
version: "1"
services:
  hello:
    image: hello:latest
    memory: 256M
```

```bash
uni compose up hello.yaml
# started hello → a3f8c2d1-...

uni compose ps hello.yaml
# SERVICE  ID              STATE
# hello    a3f8c2d1-...    running

uni compose logs hello.yaml hello
# Hello from unikernel!

uni compose down hello.yaml
# stopped hello
```
