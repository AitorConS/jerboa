---
layout: default
title: Compose
nav_order: 5
---

# Compose
{: .no_toc }

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

Jerboa compose is a small, project-local stack format implemented in `internal/compose/`.

## File Format

```yaml
version: "1"

services:
  api:
    image: api:latest
    memory: 256M
    cpus: 1
    depends_on: [db]
    networks: [app]
    ip: 10.100.0.10
    environment:
      - PORT=8080
    ports:
      - "8080:8080"
    volumes:
      - data:/var/data
    health_check: "http:8080:/healthz"
    restart: "on-failure:5"

networks:
  app:
    driver: bridge
    subnet: 10.100.0.0/24

volumes:
  data:
    size: 1G
```

## What The Parser Enforces

- `version`, if present, must be `"1"`
- at least one service is required
- every service must define `image`
- `depends_on` targets must exist
- referenced networks must exist in the top-level `networks` map
- referenced named volumes must exist in the top-level `volumes` map

## Current Runtime Semantics

- only the first network in `networks:` is actually wired to a service instance
- services reach each other by service name over the daemon's guest DNS (e.g. a `web` service connects to `db:5432`); see [Service Discovery]({% link getting-started.md %}#service-discovery-guest-dns)
- `ip:` pins a service to a static address on its network; without it the daemon's IPAM allocates one. Name resolution works either way, so `ip:` is only needed when a fixed address is required
- top-level volumes are auto-created during `compose up`
- top-level networks are auto-created during `compose up`
- `compose down --volumes` removes only volumes created by that stack
- stacks are tracked by separate `.jerboa-compose-<hash>.json` files next to their compose files; the hash identifies the full canonical compose path
- state is written atomically as resources are created, so a failed `up` can be cleaned with `down`
- repeated `down` is safe; failed cleanup retains the remaining resources in state

## Commands

```bash
jerboa compose up stack.yaml
jerboa compose ps stack.yaml
jerboa compose logs stack.yaml api
jerboa compose down stack.yaml --volumes
```

Current limits:

- `compose logs` is snapshot-only
- there is no `compose logs -f`

## Ordering

Startup order is computed with topological sort from `depends_on`.

Services are removed in alphabetical order. Startup dependencies do not currently
determine shutdown ordering.

## Upgrading Existing Stacks

Before upgrading from 0.51.1, stop existing stacks with the old CLI. Version
0.51.2 does not automatically assign a legacy `.jerboa-compose-state.json` file
to a project because that file cannot identify which compose definition owns it.

If you have already upgraded, inspect the service VM IDs recorded in the legacy
file and stop and remove those VMs explicitly. Inspect its recorded networks and volumes before
removing any resources you no longer need. Then deploy with `compose up` to
create the new state. Preserve volumes that hold data you want to keep.
