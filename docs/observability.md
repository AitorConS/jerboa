---
layout: default
title: Observability
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
jerboad --metrics-addr :9090
```

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

The daemon emits VM lifecycle spans through OTLP gRPC.

## Logging

Enable JSON logs with:

```bash
jerboad --log-format json
```

The daemon defaults to text logs.

## Dashboard

Enable with:

```bash
jerboad --ui-addr :8080
```

Routes:

- `/ui`
- `/ui/api/vms`
- `/ui/api/vm/{id}`
- `/ui/api/vm/{id}/logs`
- `/ui/api/vm/{id}/stats`

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
