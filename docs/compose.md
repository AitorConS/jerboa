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
    health_check: "tcp:PORT" | "http:PORT[:/path]"
    restart: "never" | "on-failure[:max-retries]" | "always[:max-retries]"
    replicas: <number>
    strategy: "RollingUpdate" | "Recreate"

networks:
  <network-name>:
    driver: bridge
    subnet: <CIDR, e.g. 10.100.0.0/24>

volumes:
  <volume-name>:
    size: <512M|1G|...>
```

---

## Fields Reference

### Top-level

| Field | Required | Description |
|---|---|---|
| `version` | No | Must be `"1"` if present. Omitting it is allowed — the parser treats any missing version as `"1"` |
| `services` | Yes | Map of service definitions (at least one) |
| `networks` | No | Map of network definitions |
| `volumes` | No | Map of volume definitions (auto-created on `compose up`) |

---

### Service fields

| Field | Required | Default | Description |
|---|---|---|---|
| `image` | Yes | — | Image `name:tag` from local store, or a file path to a bootable disk image (`.img`) built with `jerboa build` |
| `memory` | No | `256M` | VM memory (QEMU format: `256M`, `1G`, `4G`) |
| `cpus` | No | `1` | Number of virtual CPUs |
| `depends_on` | No | `[]` | Services that must start before this one |
| `networks` | No | `[]` | Logical networks to attach to. Only the **first** entry is actually wired up — a service connects to one managed network |
| `environment` | No | `[]` | Environment variables as `KEY=VALUE` strings |
| `ports` | No | `[]` | Port mappings: `"host:guest"`, `"host:guest/tcp"`, or `"host:guest/udp"` |
| `volumes` | No | `[]` | Volume mounts: `"name:guestpath"` or `"name:guestpath:ro"`. Each `name` must match a key under the top-level `volumes:` map |
| `health_check` | No | — | Liveness probe spec: `"tcp:PORT"` or `"http:PORT[:/path]"`. See [Health Checks]({% link architecture.md %}#health-checks) |
| `restart` | No | — | Restart policy spec: `"never"`, `"on-failure[:max-retries]"`, or `"always[:max-retries]"`. See [Restart Policies]({% link architecture.md %}#restart-policies) |
| `replicas` | No | `0` | Number of identical instances to run behind a shared service name. `replicas > 1` deploys the service as a managed [Service]({% link cli-reference.md %}#service-commands) instead of a single VM — see [Scaling with replicas](#scaling-with-replicas) below |
| `strategy` | No | — | Update strategy for scaled services: `RollingUpdate` or `Recreate`. Only meaningful when `replicas > 1` |

{: .note }
`health_check` and `restart` apply to single-instance services (`replicas` unset or `1`). For scaled services (`replicas > 1`), lifecycle is managed by `jerboa service` instead — see below.

---

### Network fields

| Field | Required | Default | Description |
|---|---|---|---|
| `driver` | No | `bridge` | Network driver. Only `bridge` is supported |
| `subnet` | No | auto-allocated | CIDR block for the network (e.g. `10.100.0.0/24`). If omitted, Jerboa auto-allocates a `/24` from its internal range |

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
    health_check: "http:8080:/healthz"
    restart: "on-failure:5"

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
    restart: always

networks:
  backend-net:
    driver: bridge
    subnet: 10.100.0.0/24
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

## Scaling with replicas

Add `replicas` to a service to run it as a group of identical, load-balanced VMs instead of a single instance:

```yaml
services:
  api:
    image: myapi:v1.0
    memory: 256M
    cpus: 1
    networks:
      - backend-net
    replicas: 3
    strategy: RollingUpdate
```

**What changes when `replicas > 1`:**

- `compose up` does **not** call `VM.Run` directly for that service. Instead it calls `Service.Run` (the same machinery behind [`jerboa service run`]({% link cli-reference.md %}#service-commands)), which creates `replicas` VMs behind the shared name `api`, attaches each to the service's first network with its own auto-allocated IP, and registers internal DNS records for all of them
- Other services can reach the group by its name — `jerboa dns resolve-all api --network backend-net` returns every replica's IP, and the daemon round-robins between them for service-to-service traffic
- `health_check` and `restart` are not set per-replica from the compose file; the service is managed as a unit instead. Use `strategy: RollingUpdate` (replace replicas one at a time) or `strategy: Recreate` (stop all, then start all) to control how `jerboa service update` rolls out changes
- The compose state file records the service under `scalable_services`, so `compose down` knows to call `Service.Remove` (which stops and deletes every replica) instead of `VM.Stop`/`VM.Remove` for a single VM
- `compose ps` and `compose logs` resolve the underlying replica VM IDs through the service so they keep working transparently

You can also manage a scaled service directly once it's running:

```bash
jerboa service ls
jerboa service inspect api
jerboa service scale api 5
jerboa service update api myapi:v1.1
```

See [Service Commands]({% link cli-reference.md %}#service-commands) for the full reference.

---

## How Dependency Ordering Works

Jerboa uses **Kahn's topological sort** algorithm to determine startup order:

1. Build a dependency graph from all `depends_on` declarations
2. Start services with no dependencies first
3. When a service finishes starting, unlock any services that depended on it

If a **dependency cycle** is detected (e.g. A depends on B, B depends on A), `jerboa compose up` will fail immediately:

```
Error: compose up: compose: dependency cycle detected
```

---

## State File

When you run `jerboa compose up stack.yaml`, a state file is created in the same directory:

```
stack.yaml
.jerboa-compose-state.json   ← automatically created
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
  ],
  "created_networks": [
    "backend-net",
    "frontend-net"
  ],
  "scalable_services": {
    "api": "api"
  }
}
```

| Field | Used for |
|---|---|
| `services` | Maps each service name to its VM ID (or, for scaled services, the service name — see `scalable_services`) |
| `service_networks` / `service_ips` | Deterministically release allocated IPs back to IPAM on `compose down` |
| `created_volumes` | Tracks which volumes `compose up` auto-created, so `compose down --volumes` knows what it's allowed to remove |
| `created_networks` | Tracks which networks `compose up` auto-created (networks that already existed are left alone and not recorded here), so `compose down` knows which ones it's safe to remove |
| `scalable_services` | Marks services deployed via `Service.Run` (`replicas > 1`); `compose down` calls `Service.Remove` for these instead of stopping a single VM — see [Scaling with replicas](#scaling-with-replicas) |

Commands `down`, `ps`, and `logs` read this file to know which VM IDs (or services) belong to the stack.

{: .warning }
Do not delete `.jerboa-compose-state.json` manually while the stack is running. If it gets lost, use `jerboa ps` to find the VM IDs and stop them individually with `jerboa stop`.

---

## Minimal Example

The simplest possible compose file — one service, no networks. The `version` field is optional and can be omitted.

Build the image first, then reference it by name:

```bash
jerboa build ./hello-linux --name hello
```

```yaml
services:
  hello:
    image: hello:latest
    memory: 256M
```

```bash
jerboa compose up hello.yaml
# started hello → a3f8c2d1-...

jerboa compose ps hello.yaml
# SERVICE  ID              STATE
# hello    a3f8c2d1-...    running

jerboa compose logs hello.yaml hello
# Hello from unikernel!

jerboa compose down hello.yaml
# stopped hello
```
