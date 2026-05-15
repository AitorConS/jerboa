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

Uni provides built-in observability for production unikernel workloads: Prometheus metrics, OpenTelemetry distributed tracing, structured JSON logging, live VM stats, and a web dashboard.

## Prometheus Metrics

Enable the metrics endpoint with `--metrics-addr` on the daemon:

```bash
unid --metrics-addr :9090
```

This starts an HTTP server with:

| Endpoint | Description |
|---|---|
| `/metrics` | Prometheus-formatted metrics |
| `/health` | Health check (returns 200 OK) |

### Available Metrics

| Metric | Type | Description |
|---|---|---|
| `uni_vms_total` | gauge | Total VMs registered |
| `uni_vms_running` | gauge | VMs currently in running state |
| `uni_vms_stopped` | gauge | VMs currently in stopped state |
| `uni_vm_lifecycle_total` | counter | VM lifecycle transitions (create, start, stop, kill, remove) |
| `uni_registry_push_total` | counter | Image push operations |
| `uni_registry_pull_total` | counter | Image pull operations |
| `uni_build_info` | gauge | Build info (version label) |

### Prometheus Scrape Config

```yaml
scrape_configs:
  - job_name: 'unid'
    static_configs:
      - targets: ['localhost:9090']
```

The daemon also updates VM state gauges every 5 seconds via an internal poller.

---

## OpenTelemetry Tracing

Enable distributed tracing with `--trace-addr`:

```bash
unid --trace-addr localhost:4317
```

This configures an OTLP gRPC exporter. When empty (default), tracing is completely disabled (no-op provider).

### VM Lifecycle Spans

The daemon creates spans for these VM lifecycle events:

| Span Name | Description |
|---|---|
| `vm.lifecycle` | Parent span for a VM operation |
| `vm.create` | VM registration |
| `vm.start` | QEMU process launch |
| `vm.stop` | Graceful shutdown |
| `vm.kill` | Immediate kill |
| `vm.remove` | VM removal from store |

### Collector Setup

Point `--trace-addr` at your OTLP collector (Jaeger, Tempo, etc.):

```bash
# Using Jaeger with OTLP collector
unid --trace-addr localhost:4317
```

---

## Structured JSON Logging

Switch from the default text format to JSON with `--log-format`:

```bash
unid --log-format json
```

### Output Format

JSON log lines include these fields:

```json
{
  "ts": "2026-05-15T12:00:00.000Z",
  "level": "INFO",
  "msg": "vm state transition",
  "vm_id": "abc123",
  "from": "created",
  "to": "starting"
}
```

JSON logs ship easily to Loki, Splunk, Datadog, or any log aggregation system.

---

## Live VM Stats

The `uni stats` command shows real-time resource usage per VM:

```bash
# One-time snapshot
uni stats <vm-id>

# Continuous watch (3s interval by default)
uni stats <vm-id> --watch

# Custom interval
uni stats <vm-id> --watch --interval 5s
```

### Available Metrics

| Metric | Description |
|---|---|
| CPU % | Percentage of CPU used by the QEMU process |
| Memory | Resident memory in bytes |
| Net RX | Total network bytes received |
| Net TX | Total network bytes transmitted |
| Source | `proc` (Linux /proc) or `fallback` (non-Linux) |

### JSON Output

```bash
uni stats <vm-id> --output json
```

---

## Web Dashboard

Enable the read-only web dashboard with `--ui-addr`:

```bash
unid --ui-addr :8080
```

### Pages

| Route | Description |
|---|---|
| `/ui` | VM list with state, health, and image |
| `/ui/vm/{id}` | VM detail page: config, health, restart info, ports, env vars, serial console log tail, live stats |

### JSON API

| Endpoint | Description |
|---|---|
| `/ui/api/vms` | List all VMs |
| `/ui/api/vm/{id}` | Full VM detail |
| `/ui/api/vm/{id}/logs` | Serial console output |
| `/ui/api/vm/{id}/stats` | Live runtime stats (CPU%, memory, network I/O) |

The VM detail page polls stats every 3 seconds and renders CPU%, memory, and network I/O inline. No JavaScript framework is required.

---

## VM Persistence (SQLite)

By default, VM state is persisted as per-VM JSON files. For improved reliability, switch to SQLite:

```bash
unid --vm-store sqlite
```

The SQLite store automatically migrates any existing `state.json` VMs on first use. Migration is idempotent — re-running does not create duplicates.

### Store Backends

| Backend | Flag | Storage | Best for |
|---|---|---|---|
| `file` | `--vm-store file` (default) | `~/.uni/vms/<id>/state.json` | Simple setups |
| `sqlite` | `--vm-store sqlite` | `~/.uni/vms/vms.db` | Production reliability |

### Health Status Persistence

Both backends persist VM health status, restart counts, and timestamps across daemon restarts. VMs that were running when the daemon exited are automatically marked as stopped with `daemon_recovered=true`.

---

## Nightly Security Scans

The nightly CI pipeline runs automated security checks:

| Tool | Check | Failure condition |
|---|---|---|
| `govulncheck` | Known Go vulnerabilities | Any vulnerability in standard library or dependencies |
| `trivy` | Filesystem CVE scan | HIGH or CRITICAL severity findings |

These run in `.github/workflows/nightly.yml` at 02:00 UTC daily.