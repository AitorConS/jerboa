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
you pass `--insecure`. Scrapers send it as `Authorization: Bearer <token>`.
`jerboa daemon start --metrics-addr ... --ui-addr ...` generates the token and
stores it as `observability_token` in `~/.jerboa/config.toml`.

Endpoints:

- `/metrics`
- `/health`

The repo currently exports daemon/VM/image/network counters and gauges, including:

- VM state gauges
- VM lifecycle counters
- image count
- active port-forward count
- bridge count
- daemon build info
- daemon start time

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
is sent to `/ui/login`, which stores the
token in an HttpOnly cookie. The dashboard lists which environment variables a
VM has, not their values.

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
Gossip is plain HTTP, so keep it on a private network.

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

The daemon migrates file-backed VM state into SQLite on first use.
