---
layout: default
title: Observability
description: "jerboa's observability: the daemon's /metrics and /health endpoints, the metrics it exports and how to enable them."
nav_order: 7
---

# Observability
{: .no_toc }

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

Jerboa exposes observability from the daemon, not the CLI.

## Metrics

Enable with:

```bash
jerboad --metrics-addr 127.0.0.1:9090
```

On loopback metrics need no token, and requests must name `localhost` or a
loopback address in `Host`, so a web page cannot read the endpoint through DNS
rebinding. To serve metrics on another address, or the dashboard at all, give
the daemon a read-only token with `--observability-token-file` (a `0600` file)
or `JERBOA_OBSERVABILITY_TOKEN`; without one the daemon refuses to start, unless
you pass `--insecure`. Off loopback the token must be at least 32 characters
(`openssl rand -hex 32`). Scrapers send it as `Authorization: Bearer <token>`.
An address that sends 10 wrong tokens gets `429 Too Many Requests`, without
its token being checked, until it slows down to one try a second.
`jerboa daemon start --metrics-addr ... --ui-addr ...` generates the token and
stores it as `observability_token` in `~/.jerboa/config.toml`.

Endpoints:

- `/metrics`
- `/health`

The repo currently exports daemon/VM/image/network counters and gauges, including:

| Metric | What it counts |
|---|---|
| `jerboa_vms_created_total`, `jerboa_vms_starting_total`, `jerboa_vms_running_total`, `jerboa_vms_stopping_total`, `jerboa_vms_stopped_total` | VMs currently in each state (gauges, despite the suffix) |
| `jerboa_vm_starts_total`, `jerboa_vm_stops_total`, `jerboa_vm_restarts_total`, `jerboa_vm_errors_total` | VM lifecycle events |
| `jerboa_images_total` | Images in the store |
| `jerboa_push_total`, `jerboa_pull_total`, `jerboa_push_errors_total`, `jerboa_pull_errors_total` | Registry pushes and pulls, and the ones that failed |
| `jerboa_port_forwards_active` | Published ports being forwarded |
| `jerboa_bridge_count` | Network bridges |
| `jerboa_build_info` | The daemon's version, as labels |
| `jerboa_start_time_seconds` | When the daemon started |

## Tracing

Enable with:

```bash
jerboad --trace-addr localhost:4317
```

The daemon emits VM lifecycle spans through OTLP gRPC. Spans go to a collector
on this host in plain text and to any other host over TLS; set
`OTEL_EXPORTER_OTLP_INSECURE=true` to send them unencrypted anyway.

## Logging

Enable JSON logs with:

```bash
jerboad --log-format json
```

The daemon defaults to text logs.

## Dashboard

Enable with:

```bash
jerboad --ui-addr 127.0.0.1:8080 --observability-token-file /etc/jerboa/observability-token
```

The dashboard needs the token even on loopback, since every local user can
reach `127.0.0.1` and it shows VM names, addresses and serial logs. A browser
is sent to `/ui/login`, which checks the token and sets an HttpOnly session
cookie named after the dashboard's port (`jerboa_ui_8080`). The cookie never
holds the token: browsers send a cookie to every port of a host, so it only
opens this dashboard, for 12 hours or until the daemon restarts, and is not
accepted by `/metrics`, which takes the token only as
`Authorization: Bearer`. The session is also bound to the address the browser
signed in from (all loopback addresses count as one), so a cookie replayed
from anywhere else is refused with 401; a browser whose address changes signs
in again. Requests from an address inside a jerboa network's subnet, which is
where guests connect from, are refused with 403 whatever they carry. The
dashboard lists which environment variables a VM has, not their values.

Sign-in attempts are checked one at a time and a wrong one takes half a
second, so the token cannot be guessed faster than two tries a second however
many run in parallel. An attempt that waits more than 10 seconds for its turn
gets `429`; try again. A bearer token sent to the dashboard is limited per
address as on `/metrics`.

Routes:

- `/ui`
- `/ui/api/vms`
- `/ui/api/vm/{id}`
- `/ui/api/vm/{id}/logs`
- `/ui/api/vm/{id}/stats`

## Cluster

`--cluster-addr` serves the gossip endpoint that other nodes join. Every node
needs the same token, from `--cluster-token-file` or `JERBOA_CLUSTER_TOKEN`; the
daemon refuses to serve gossip without one unless you pass `--insecure-cluster`.
The token must be at least 32 characters (`openssl rand -hex 32`), and an
address that sends 10 wrong tokens gets `429` until it slows down to one try a
second. Gossip is plain HTTP, so keep it on a private network.

The token proves that a caller belongs to the cluster, not which node it is,
so a node never lets a peer's claim rewrite another member's record. A known
member's address does not change over gossip (a restarted node joins under a
new ID), and a member is marked `left` only when it says so itself in a reply
from its recorded address. No node takes another's word that a member is
dead: each one declares a member dead when it has had no news of it, first-hand
or relayed, for 30 seconds. A member marked dead comes back when it gossips
again or a peer relays newer news of it; one that left does not.

A node only sends gossip, including `--join` requests, to addresses that pass a
fixed policy, checked again on every connection after DNS resolution:
link-local (cloud metadata), unspecified and multicast addresses are refused,
and so is loopback unless the node itself advertises a loopback address (as
`--cluster-addr :7946` does, for a single-host cluster). Gossip requests never
follow redirects and ignore `HTTP_PROXY`/`HTTPS_PROXY`. Gossip messages and
replies are limited to 1 MiB and 1024 members.

## VM Stats

CLI command:

```bash
jerboa stats <id>
jerboa stats <id> --watch
```

Linux collects live process-backed stats. Non-Linux paths fall back to a zeroed `fallback` source.

## Serial Log Retention

The daemon keeps an in-memory serial log buffer per VM.

Daemon flag:

```bash
jerboad --vm-log-max-bytes 8388608
```

`0` keeps the built-in default of 4 MiB per VM.

`jerboa logs -f` follows the retained log buffer until the VM stops.

## SQLite VM Store

Use:

```bash
jerboad --vm-store sqlite
```

The daemon migrates file-backed VM state into SQLite on first use. The file
registry is left as it was, so going back to `--vm-store file` finds the
records it had. Each VM is taken over once: the database remembers which ones
it migrated, and a VM you remove afterwards does not come back from the file
it left behind. The database file is created readable by its owner only.
