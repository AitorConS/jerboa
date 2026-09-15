---
layout: default
title: Firecracker on macOS
parent: macOS Apple Silicon
nav_order: 10
---

# Firecracker on macOS

Jerboa supports the native ARM64 Hypervisor.framework backend from the sibling
`../firecracker-macos` checkout. Use its signed macOS executable, not an upstream
Linux Firecracker binary. The fork requires macOS 26 and Apple Silicon; this
integration was exercised on the local M5 host. Source configurations retain QEMU as the default; the native package installer selects Firecracker.

## Install a published native release

On macOS 26+ in a native Apple Silicon terminal:

```sh
curl -fsSL https://jerboa.dev/install.sh | bash
# Or select a released version:
curl -fsSL https://jerboa.dev/install.sh | bash -s -- --version VERSION
jerboa status
```

Run without `sudo`; the script requests elevation only to install the package
and CLI wrappers. Homebrew supplies `jq` and `minisign` if they are missing.
The installer detects macOS/ARM64, verifies the signed release manifest and
package SHA-256, checks the Developer ID signature and Gatekeeper assessment,
then installs the complete package at `/usr/local/libexec/jerboa`. Wrappers in
`/usr/local/bin` preserve relative library and tool lookup. It selects
Firecracker in the user's config and starts the user's launchd daemon.
Stop running VMs before upgrading; the installer stops the managed daemon.

This route requires a published release whose signed manifest contains the
`macos` component. Older Linux-only releases are rejected before installation.
The CI publishing path is implemented; its first macOS publication requires a
provisioned macOS runner and Apple signing identities. Until that release is
published, use the source build below. See the
[distribution pipeline]({% link macos-distribution.md %}) for prerequisites.

## Build and run

Build Jerboa and the ARM64 kernel with `./scripts/build-macos.sh`. If the native
tools already exist, rebuilding only the CLI and daemon is sufficient:

```sh
go build -o dist/macos-arm64/ ./cmd/jerboa ./cmd/jerboad
export JERBOA_FIRECRACKER_BIN="$PWD/../firecracker-macos/build/macos-arm64/firecracker"
./dist/macos-arm64/jerboa config set hypervisor firecracker
./dist/macos-arm64/jerboa daemon start
./dist/macos-arm64/jerboa build examples/hello --platform linux/arm64 --name hello
./dist/macos-arm64/jerboa run hello:latest --attach
```

Stop existing VMs and the managed daemon before changing backends. To select
Firecracker for a single managed launch, use `daemon start --hypervisor firecracker`.
The launcher resolves `JERBOA_FIRECRACKER_BIN`, then a `firecracker` executable
beside `jerboad`, then PATH or `/opt/homebrew/bin`. It records the absolute path
in the launchd service. Keep the fork and its native dependencies in place.

Foreground operation permits explicit kernel and policy selection:

```sh
./dist/macos-arm64/jerboad \
  --hypervisor firecracker \
  --fc-bin "$PWD/../firecracker-macos/build/macos-arm64/firecracker" \
  --tools-dir "$PWD/dist/macos-arm64/tools"
```

On macOS the default kernel is `tools/kernel.img`, an ARM64 ELF. `--fc-kernel`
overrides it. Jerboa does not download the Linux/x86 Firecracker kernel for this
backend. The executable retains the fork's own code-signing and entitlement
requirements; Jerboa does not rebuild or re-sign it.

## Supported behavior

ARM64 ELF guests can use 1-4 vCPUs, 64-2048 MiB of RAM, an ephemeral root disk,
and up to three persistent volumes. Environment, static network configuration and
mount labels are delivered through `opt/uni/*` firmware files. Root disk writes
never modify the image-store copy.

Without `--network`, existing private slirp configurations keep guest IP
`10.0.2.15` and VMM-owned TCP/UDP publishing. Their health checks still require
a published TCP port.

With `--network`, Firecracker connects its generic Unix Ethernet transport to
Jerboa's shared gVisor stack and Ethernet switch. QEMU is not required. Jerboa
allocates guest IPs from the network subnet, accepts `--ip`, and provides direct
VM-to-VM communication and DNS names/aliases within that network. Overlapping
subnets and duplicate allocated IPs are rejected. TCP/HTTP health checks dial
the guest through the stack, including ports that are not published.

TCP/UDP publishing supports explicit IPv4 bind addresses. An omitted address
binds `0.0.0.0` on all host IPv4 interfaces, for both slirp and shared
networks; use `127.0.0.1:HOST:GUEST` for local access. Slirp grants exactly
that address/protocol/nonzero port in `security.listeners`. A wildcard
listener grants no egress or DNS permission and does not match a concrete bind.
Darwin TCP `SO_REUSEADDR` can allow a concrete bind to coexist with a wildcard
bind on the same port (the concrete bind takes precedence); identical listener
endpoints collide. Binding collisions
fail startup and roll back the new VM's resources. Ports, accepted connections,
outbound sockets and the private link are closed when that VM stops, even if
other VMs keep the shared network alive. The daemon recreates links, forwarding,
DNS and health checks after replacement, and the VMM reconnects automatically.
Established connections may break and applications must retry.

Console output and VMM diagnostics are available through `jerboa logs`. The
manager observes guest exit separately from the persistent API supervisor,
preserving `on-failure` restart behavior. Existing VMs can be adopted after a
daemon crash; automatic restart policies are not applied to adopted VMs, matching
Jerboa's existing adoption behavior. Console history is not recovered.

## Network policy and limits

Outbound traffic and external DNS are denied by default. Joining a named
network explicitly enables communication and service DNS inside that network.
Supply a JSON
security object with `jerboad --fc-security /absolute/path/policy.json`, or set
`JERBOA_FIRECRACKER_SECURITY` when starting the managed service. For example:

```json
{
  "version": 1,
  "egress": [{"protocol": "tcp", "address": "192.0.2.10", "port": 443}],
  "dns": {"address": "1.1.1.1", "port": 53}
}
```

Replace the example destination with the actual endpoint needed by the guest.
Policy is shared by this daemon's Firecracker VMs. Slirp receives these rules
in the VMM and adds published listener permissions. For shared networks,
Jerboa enforces exact IPv4/protocol/port egress rules and uses only `dns` as its
external resolver. `JERBOA_DNS_UPSTREAM` does not override Firecracker's policy.
Service DNS is answered locally. Access through the network gateway to the
host is translated to `127.0.0.1`, so authorize that address and the required
port explicitly. Granting DNS does not authorize TCP/UDP to resolved addresses.

Shared networking requires version 1 hardened mode. It rejects development
mode, unknown policy fields and manually supplied `listeners`; publish host
listeners using port mappings. Jerboa gives the VMM only the exact private
Unix socket capability. Seatbelt remains enabled in the VMM and broker;
Internet sockets are owned by the policy-enforcing daemon.

The shared stack binds each Ethernet link to its assigned source MAC/IP,
rejects source spoofing, IPv6 and VLAN frames, validates framing before
allocating, and bounds each link's pending output to 64 frames. The transport
limit is 65536 bytes per frame; the stack MTU is 1500. Shared host connections
(published and outbound, including pending dials) are capped at 64 per VM and
256 per network. UDP flows expire after 30 seconds without response activity;
TCP dials time out after five seconds. DNS has bounded query workers. Removing
a VM cancels its pending dials and closes its live host connections.

- One IPv4 interface per VM. Custom IPs require a named network; TAP and
  multi-network attachments are explicitly rejected. The gateway is the first
  subnet host; custom Compose IPAM gateways are rejected.
- Jerboa provides snapshot CLI commands and RPCs for RAM, CPU/device state and
  the ephemeral root disk on named networks. Restore resumes the same stopped
  VM in place and recreates its network identity, DNS and published ports.
  VMs with volumes are rejected; snapshots are local-development artifacts,
  not portable backups. Established TCP connections are not preserved. See
  [snapshot requirements and acceptance]({% link macos-snapshots.md %}).
- x86 emulation requires QEMU; Firecracker rejects `--emulate-x86`.
- Stop requests a VirtIO power-button shutdown. The updated Nanos kernel sends
  SIGTERM and synchronizes kernel-held writes before powering off. A compatible
  updated fork binary is required alongside this kernel. The VMM forces exit
  after 35 seconds, with a separate 40-second Jerboa bound; force-stop/kill and
  timeout do not guarantee a flush. Application-only buffers still require
  application cleanup. See [shutdown behavior]({% link macos-shutdown.md %}).
- Host CPU shares and memory watchdog limits are rejected. Disk limits map to
  the fork's aggregate device limits: 1-1000000 IOPS and 1 MiB/s-1 TiB/s.
  Omitted values retain the fork's defaults, not unlimited I/O.
- Native Firecracker statistics aggregate the live supervisor/VMM process
  tree, with persistent CPU deltas and PID/start-time identity checks. RSS is
  host resident memory, not guest-internal used RAM. Native accounting requires
  cgo; see [statistics semantics]({% link macos-stats.md %}).

## Verification

```sh
go test ./...
python3 scripts/test-firecracker-macos.py
JERBOA_TEST_BIN="$PWD/dist/macos-arm64" python3 scripts/test-firecracker-network.py
```

The standalone smoke uses a temporary home, builds its own ARM64 guest through
Jerboa, and exercises one/four CPU boots, environment, persistent volumes,
TCP/UDP, occupied ports, guest exit, daemon recovery, failure restart and stop.
It uses `JERBOA_FIRECRACKER_BIN` or the sibling fork's build path. It does not
replace the user's managed daemon.

An additional manager-level test can use the fork's optional Jerboa demo disk
(HTTP 8080, UDP echo 8081, `MARKER=hvf-smoke`, `/shutdown`):

```sh
JERBOA_TEST_FC_BIN="$PWD/../firecracker-macos/build/macos-arm64/firecracker" \
JERBOA_TEST_FC_KERNEL="$PWD/dist/macos-arm64/tools/kernel.img" \
JERBOA_TEST_FC_DISK="$PWD/../firecracker-macos/build/macos-arm64/demo/root.img" \
go test ./internal/vm -run TestNativeFC -v
```

## Shared networks and Compose

```sh
jerboa network create app --subnet 172.25.0.0/24
jerboa run service:latest --name postgres --network app --ip 172.25.0.10 \
  --health-check tcp:5432
jerboa run backend:latest --network app -e DATABASE_HOST=postgres \
  -p 127.0.0.1:8080:8080
```

Both images must contain ARM64 executables compatible with the Jerboa kernel.
A working network does not make an x86 PostgreSQL package executable on HVF.
The OPS PostgreSQL packages consulted on 2026-09-13 are x86_64. A separate
[ARM64 PostgreSQL fixture]({% link postgres-arm64.md %}) now builds a pinned
experimental pthread fork and tests real SQL from another guest, including
volume persistence after server replacement. It is not stock PostgreSQL or a
production/durability qualification. The general shared-network regression
still uses an ARM64 HTTP/UDP guest and reports that distinction explicitly.

Compose accepts the existing network list or an attachment mapping:

```yaml
services:
  postgres:
    image: postgres-arm64:latest
    health_check: tcp:5432
    networks:
      app:
        ipv4_address: 172.25.0.10
        aliases: [database]
  backend:
    image: backend-arm64:latest
    depends_on: [postgres]
    networks: [app]
    environment: [DATABASE_HOST=database, DATABASE_PORT=5432]
    ports: ['127.0.0.1:8080:8080']
networks:
  app:
    ipam:
      config:
        - subnet: 172.25.0.0/24
```

Services without attachments join an implicit network whose generated name is
scoped to the Compose file. Explicitly declared network names keep Jerboa's
existing global naming semantics. This remains
Jerboa's Compose subset, not a promise to accept every Docker Compose option.
Only one attachment/subnet is supported. Names and aliases are network-scoped
and persisted in VM configuration (`network_aliases` in the Run API).
A literal name/alias in the caller's network takes priority over stripping a
network suffix: `database.app` assigned to `postgres` wins over service
`database` in network `app`. If no literal match exists, `host.network`
is tried within that same scope. Explicit caller scope never switches networks.
Unscoped dotted queries infer their scope from the final suffix.

For an isolated build, put the rebuilt CLI and daemon in a separate directory
and link its `tools` directory to the existing ARM64 tools. Set
`JERBOA_TEST_BIN` to that directory and `JERBOA_FIRECRACKER_BIN` to the signed
fork executable. `test-firecracker-network.py` covers IPAM, IP/name/alias
traffic, isolation, unpublished health, TCP/UDP bindings and collisions,
daemon SIGKILL/restart, Compose and default/explicit outbound and DNS policies.
The tests use a temporary HOME and a separate daemon socket.
