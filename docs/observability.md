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
jerboad --metrics-addr :9090
```

This starts an HTTP server with:

| Endpoint | Description |
|---|---|
| `/metrics` | Prometheus-formatted metrics |
| `/health` | Health check (returns 200 OK) |

### Available Metrics

| Metric | Type | Description |
|---|---|---|
| `uni_vms_created_total` | gauge | Number of VMs in created state |
| `uni_vms_starting_total` | gauge | Number of VMs in starting state |
| `uni_vms_running_total` | gauge | Number of VMs in running state |
| `uni_vms_stopping_total` | gauge | Number of VMs in stopping state |
| `uni_vms_stopped_total` | gauge | Number of VMs in stopped state |
| `uni_vm_starts_total` | counter | Total number of VM start operations |
| `uni_vm_stops_total` | counter | Total number of VM stop operations |
| `uni_vm_restarts_total` | counter | Total number of VM restart operations |
| `uni_vm_errors_total` | counter | Total number of VM errors |
| `uni_build_info` | gauge | Build information for the daemon (`version` label) |
| `uni_images_total` | gauge | Number of locally stored images |
| `uni_push_total` / `uni_pull_total` | counter | Total number of image push/pull operations |
| `uni_push_errors_total` / `uni_pull_errors_total` | counter | Total number of image push/pull errors |
| `uni_port_forwards_active` | gauge | Number of active port forwarding rules |
| `uni_bridge_count` | gauge | Number of active network bridges |
| `uni_start_time_seconds` | gauge | Unix timestamp of daemon start time |

{: .note }
The five `uni_vms_*_total` gauges reflect a live snapshot of the VM registry (how many VMs are currently in each state), refreshed every 5 seconds — they are not cumulative counts despite the `_total` suffix. The `uni_vm_*_total` counters (starts, stops, restarts, errors) are the cumulative lifecycle counters.

### Prometheus Scrape Config

```yaml
scrape_configs:
  - job_name: 'jerboad'
    static_configs:
      - targets: ['localhost:9090']
```

The daemon also updates VM state gauges every 5 seconds via an internal poller.

---

## OpenTelemetry Tracing

Enable distributed tracing with `--trace-addr`:

```bash
jerboad --trace-addr localhost:4317
```

This configures an OTLP gRPC exporter. When empty (default), tracing is completely disabled (no-op provider).

### VM Lifecycle Spans

The daemon creates spans for these VM lifecycle events:

| Span Name | Description |
|---|---|
| `vm.lifecycle` | Parent span for a VM state transition (carries `vm.id`, `vm.state`, `vm.event` attributes) |
| `vm.create` | VM registration (carries `vm.image`, `vm.memory`, `vm.cpus`, and optionally `vm.name`/`vm.network`) |
| `vm.start` | QEMU process launch |
| `vm.stop` | Graceful shutdown |
| `vm.kill` | Immediate kill |
| `vm.signal` | Sending an arbitrary signal to the VM |
| `vm.remove` | VM removal from store |
| `vm.monitor` | Background QEMU process monitoring |

### Collector Setup

Point `--trace-addr` at your OTLP collector (Jaeger, Tempo, etc.):

```bash
# Using Jaeger with OTLP collector
jerboad --trace-addr localhost:4317
```

---

## Structured JSON Logging

Switch from the default text format to JSON with `--log-format`:

```bash
jerboad --log-format json
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

The `jerboa stats` command shows real-time resource usage per VM:

```bash
# One-time snapshot
jerboa stats <vm-id>

# Continuous watch (2s interval by default)
jerboa stats <vm-id> --watch

# Custom interval
jerboa stats <vm-id> --watch --interval 5s
```

### Available Metrics

| Metric | Description |
|---|---|
| CPU % | Percentage of CPU used by the QEMU process |
| Memory | Resident memory in bytes |
| Net RX | Total network bytes received |
| Net TX | Total network bytes transmitted |
| Source | `procfs` (Linux `/proc`) or `fallback` (non-Linux, all values zero) |

### JSON Output

```bash
jerboa stats <vm-id> --output json
```

---

## Web Dashboard

Enable the read-only web dashboard with `--ui-addr`:

```bash
jerboad --ui-addr :8080
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
jerboad --vm-store sqlite
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

---

## Resource Quotas

When running on Linux with cgroup v2, you can enforce CPU and memory limits per VM:

```bash
# Limit CPU shares (1-10000 cgroup v2 weight, default 100)
jerboa run myapp:latest --cpu-shares 512

# Set a memory hard limit (K/M/G suffixes)
jerboa run myapp:latest --memory-max 1G
```

If cgroup v2 is not available, the flags are accepted but no limits are enforced (a warning is logged). See [Resource Quotas]({% link architecture.md %}#resource-quotas) in the architecture reference for how the daemon manages cgroups.

## I/O Throttling

Disk I/O for the **boot disk only** (not mounted volumes) can be limited using QEMU's native drive throttle:

```bash
# Limit to 1000 IOPS
jerboa run myapp:latest --disk-iops 1000

# Limit throughput to 10MB/s
jerboa run myapp:latest --disk-bps 10M
```

See [I/O Throttling]({% link architecture.md %}#io-throttling) in the architecture reference for details.