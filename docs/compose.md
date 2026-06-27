---
layout: default
title: Compose
nav_order: 4
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
    environment:
      - PORT=8080
    ports:
      - "8080:8080"
    volumes:
      - data:/var/data
    health_check: "http:8080:/healthz"
    restart: "on-failure:5"
    replicas: 3
    strategy: RollingUpdate

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
- `replicas` must be non-negative
- `strategy` must be `RollingUpdate` or `Recreate`

## Current Runtime Semantics

- only the first network in `networks:` is actually wired to a service instance
- top-level volumes are auto-created during `compose up`
- top-level networks are auto-created during `compose up`
- `compose down --volumes` removes only volumes created by that stack
- stacks are tracked by a local state file named `.jerboa-compose-state.json` next to the compose file

## Replicas

`replicas > 1` switches the service from single-VM orchestration to the service manager.

That means:

- `compose up` uses `Service.Run`
- `compose down` uses `Service.Remove`
- the compose state tracks the scalable service separately

Single-instance services still go through the VM API directly.

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
- health checks and restart config are applied directly only to single-instance services

## Ordering

Startup order is computed with topological sort from `depends_on`.

Shutdown order is the reverse of the recorded startup ordering.
