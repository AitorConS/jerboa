---
layout: default
title: Benchmarks
nav_order: 16
---

# Benchmarks
{: .no_toc }

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

`jerboa-bench` compares Jerboa hypervisor backends with each other and with
Docker. It is built to give numbers you can defend:

- **One binary everywhere.** A static test service (`cmd/jerboa-bench/testapp`)
  is built once. The harness packages it as a Jerboa image and as a
  `FROM scratch` Docker image, then checks the SHA-256 of the binary inside each
  image. It refuses to run if they differ.
- **Repeated, randomized runs.** Warmup rounds come first. The measured runs of
  all runtimes are then shuffled together with a seed, so drift on the host
  (thermal throttling, page cache, background jobs) does not favor one runtime.
  Summaries report the median with a bootstrap 95% confidence interval, plus
  p90, p99 and mean±SD.
- **Separate phases.** Each run records `create` (the CLI returns), `ready`
  (the first successful probe through the published port), `stop` and
  `remove` separately.
- **Separate sizes.** Each image reports its logical size, allocated size and
  compressed download size (gzip, plus zstd -19 when `zstd` is installed).

## Running it

```sh
go build -o dist/jerboa-bench ./cmd/jerboa-bench

# Linux: two daemons with separate state, one network per daemon.
HOME=/tmp/bq jerboad --host unix:///tmp/bq.sock &
HOME=/tmp/bf jerboad --host unix:///tmp/bf.sock --hypervisor firecracker &
HOME=/tmp/bq jerboa --host unix:///tmp/bq.sock network create benchq --subnet 10.231.0.0/24
HOME=/tmp/bf jerboa --host unix:///tmp/bf.sock network create benchfc --subnet 10.232.0.0/24

dist/jerboa-bench --jerboa dist/jerboa \
  --runtime qemu=jerboa,host=unix:///tmp/bq.sock,network=benchq,store=/tmp/bq/.jerboa/images \
  --runtime fc=jerboa,host=unix:///tmp/bf.sock,network=benchfc,store=/tmp/bf/.jerboa/images,layout=compact \
  --runtime docker=docker \
  --runs 30 --warmup 3 --idle-sample 10s --load-duration 10s --load-direct \
  --out bench.json
```

The run writes `bench.json` (metadata, image sizes, summaries and every run),
`bench.csv` (one row per run) and prints a summary table. The nightly
`image-benchmarks` job runs the same harness on the KVM runner and uploads
these files as artifacts.

### Runtime options

`--runtime label=kind[,key=value...]` is repeatable. `kind` is `jerboa` or
`docker`.

| Option | Applies to | Meaning |
|---|---|---|
| `host` | jerboa | Daemon endpoint |
| `network` | jerboa | Network to join (required on Linux for port publishing) |
| `store` | jerboa | The daemon's `--store`, used to measure image sizes (default `~/.jerboa/images`) |
| `layout` | jerboa | `standard` or `compact` |
| `bin` | jerboa | A different `jerboa` CLI for this runtime |
| `image` | both | Use a prebuilt image instead of the test app (sizes and binary verification are skipped) |
| `args` | both | Extra, space-separated `run` flags, e.g. `args=--disk-io-engine async` or `args=-e POSTGRES_HOST_AUTH_METHOD=trust` |

## What the numbers mean

| Metric | Jerboa | Docker |
|---|---|---|
| `create_ms` | `jerboa run` returns (image resolved, boot disk cloned and verified, hypervisor launched) | `docker run -d` returns |
| `ready_ms` | First 2xx from `--ready` (default `http:/ready`) through the published port, measured from the start of `create` | Same |
| `stop_ms` / `remove_ms` | `jerboa stop` / `jerboa rm` | `docker stop` / `docker rm` |
| `mem_*_mib` | Resident memory of the hypervisor process (guest RAM it has touched plus VMM overhead) | Container cgroup usage from `docker stats` |
| Image `logical` | Disk image size | `docker image inspect` size |
| Image `allocated` | Blocks the disk image occupies on the host | — |
| Image `gzip` / `zstd` | Compressed disk image | Compressed `docker save` output |

Memory figures measure different boundaries. A VM's resident set includes its
guest kernel and page cache, while a container's cgroup excludes the shared host
kernel. Report both definitions together, not as a single "memory" column.

On macOS, Docker Desktop runs containers inside a Linux VM, and Jerboa's native
backends are a development preview. Publish comparisons from Linux hosts.

## Service workloads

### HTTP and published-port overhead

`--load-duration` runs a closed-loop HTTP load test (`--load-concurrency`
keep-alive connections against `--load-path`) after each measured run is
ready. With `--load-direct` on Linux, it also loads the guest or container IP
directly. The difference between `load_published_*` and `load_direct_*` is the
cost of port publishing: Jerboa's userspace forwarder versus Docker's proxy or
NAT.

### Databases

`--load-cmd` replaces the HTTP test with any command. `{host}` and `{port}` are
substituted, and `--load-metric` extracts the result from the command's output.
For example, PostgreSQL with pgbench:

```sh
dist/jerboa-bench --jerboa dist/jerboa --guest-port 5432 --ready tcp \
  --runtime pg=jerboa,host=unix:///tmp/bq.sock,network=benchq,image=postgresql:latest \
  --runtime docker=docker,image=postgres:11,args=-e POSTGRES_HOST_AUTH_METHOD=trust \
  --runs 10 --warmup 1 \
  --load-cmd 'pgbench -h {host} -p {port} -U postgres -i -s 10 postgres >/dev/null && pgbench -h {host} -p {port} -U postgres -c 8 -j 4 -T 30 postgres'
```

A TCP probe can succeed on a published port before the database accepts
queries, so keep the initialization step (`pgbench -i`) in the command.

## Profiling storage

- `jerboad --fc-metrics-dir DIR` (Linux Firecracker) writes each VM's device
  metrics to `DIR/fc-<id>-metrics.json`. They include per-drive read and write
  bytes and counts, flush counts, and latency aggregates.
- Compare `--disk-io-engine sync` with `async`, and `--volume-cache writeback`
  with `unsafe`, as separate runtimes (`args=...`) in one run. They then share
  the same randomized schedule.
- `jerboa build --size-report` shows which programs, packages, npm modules
  and Python distributions make up an interpreted-language image.
