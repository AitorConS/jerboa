---
layout: default
title: Firecracker snapshots on macOS
nav_order: 13
---

# Firecracker snapshots on macOS

`jerboad` can capture a running Firecracker/HVF VM into a private store and
restore it **in place**: the same stopped VM resumes from the captured instant
with its ID, MAC, IP, name, aliases and published ports. This is a deliberately
narrow v1 for local development on the Mac that took the snapshot. It is not a
clone, migration, backup or crash-consistency mechanism, and it is not approved
for production use.

## Requirements and scope

- Apple Silicon, the supported macOS 26 build, and `jerboad
  --hypervisor firecracker` with a snapshot-capable `firecracker-macos` build.
- The VM is attached to a named Jerboa network (`unix-stream` switch link).
  VMs on the default slirp network are rejected with `UNSUPPORTED_CAPABILITY`.
- The VM has **no Jerboa volumes**. A volume is external state the snapshot
  does not contain, so its consistency with guest RAM cannot be guaranteed;
  such VMs are rejected.
- Restore targets the VM the snapshot was taken from, while it is stopped.
- Other backends (QEMU, Linux Firecracker) report `UNSUPPORTED_CAPABILITY`.

## CLI and API

```sh
jerboa snapshot create <vm> <name>    # pause, capture, resume
jerboa snapshot restore <vm> <name>   # stopped VM -> running, in place
jerboa snapshot ls [--output json]
jerboa snapshot inspect <name>
jerboa snapshot rm <name>
```

JSON-RPC methods: `VM.SnapshotCreate {id, name}` returns `SnapshotInfo`,
`VM.SnapshotRestore {id, name}` returns `VMInfo`, and `Snapshot.List {}`,
`Snapshot.Inspect {name}`, `Snapshot.Remove {name}`. Create and restore block
until they finish. Daemon shutdown cancels them; a client disconnect does not.

Names are 1-63 characters of lowercase letters, digits, `.`, `_` and `-`,
starting with a letter or digit. The client never chooses a path: the daemon
resolves names only inside its store.

## What is preserved

| Preserved from the snapshot | Recreated from the VM and current daemon |
| --- | --- |
| Guest RAM, vCPU/GIC and VirtIO state | Switch link and a fresh private socket |
| Ephemeral root disk contents | Service DNS, name and aliases |
| Guest-visible MAC (checked, not changed) | Published ports and health checks |
| CPU, memory and VMM limits | Network policy (egress/DNS) |

The guest does not reboot; application state in memory resumes at the capture
instant. Established TCP connections and switch flow tables are **not**
preserved: clients must reconnect. After the restored incarnation stops, a
normal `VM.Start` boots the base image as usual. `VM.Start` on a stopped VM
creates a replacement with a new ID, so snapshots of the old ID cannot be
restored into it.

## Store

Snapshots live in `~/.jerboa/snapshots` (`jerboad --snapshot-dir`). Each entry
is a `0700` directory containing the fork artifact (`vm.snapshot`) and an
atomically written sidecar, `jerboa-snapshot.json`. The sidecar records the VM
ID and name, backend, image, CPU/memory, a configuration fingerprint
(environment values are hashed, not stored), network name, subnet, gateway, IP,
MAC and aliases, published ports, fork compatibility data, per-component sizes
and SHA-256 digests, and the total size. It never stores socket or temporary
paths.

- Restore re-hashes every component and requires it to match both the fork
  manifest and the sidecar. Every file must be private (`0600`), owned by the
  daemon user, single-link and not a symlink. Unexpected entries are rejected.
  The fork repeats its own compatibility and integrity checks on load.
- Quotas apply before capture, using an estimate (RAM + root disk + kernel),
  and again at publication with the real size. Defaults: 16 snapshots and
  32 GiB total (`--snapshot-max-count`, `--snapshot-max-bytes`, 0 disables).
- Captures are staged under `.staging` and renamed into place only after the
  sidecar is flushed. Removal first moves the entry into staging. Incomplete
  staging is deleted at daemon start.
- `snapshot rm` refuses a snapshot a restore is using, including one referenced
  by an interrupted restore that has not been recovered yet.
- A snapshot contains guest RAM and firmware data, including environment
  variables passed to the VM. Treat the store as secret.

## Lifecycle coordination

Create pauses the guest through the supervisor API, captures it, publishes the
entry and always resumes it. If resume fails, the VM is force-stopped and the
error says so. The daemon never reports a paused guest as running.

Restore runs under the same daemon lock as `VM.Run` and `VM.Start`. It:

1. validates the snapshot, the VM's identity and configuration fingerprint,
   the exact MAC, and that the registered network keeps its subnet and
   gateway, the IP belongs to this VM, and no other live VM on that network
   holds its name or aliases;
2. marks the VM `restoring` (persisted) and prepares the switch link with the
   same MAC, a new socket, DNS and the original published ports;
3. starts a supervisor without a guest, sets the `unix-stream` interface and
   socket capability, loads the snapshot, waits for `Paused` and resumes;
4. persists the VM as `running` before confirming success, then starts health
   checks, statistics and the normal exit monitor. A persistence error triggers
   rollback instead of returning success.

Any failure (port or alias collision, integrity, load or resume error) stops
only the supervisor this restore launched, closes the link and publications,
and returns the VM to `stopped`.

A per-VM lifecycle claim is acquired by start, stop, kill, removal, snapshot
operations and automatic restart. Signals use the stop/kill lifecycle too.
Conflicting operations fail with "vm lifecycle operation in progress"; the
registry is rechecked after acquisition to reject an already removed VM. An automatic
restart waiting in backoff blocks restore, and a restart skips a VM that is no
longer stopped.

### Crash recovery

Create and restore write a durable journal record in `.operations` before each
externally visible effect (pause, capture, publication, supervisor launch,
load, resume). At start-up, before adopting running VMs, the daemon:

- adopts an interrupted restore whose own supervisor (matched by PID and
  VM-specific socket in its command line) already reports `Running`;
- otherwise stops that supervisor and rolls the VM back to `stopped`, with a
  warning in `jerboa inspect`;
- resumes a guest left paused by an interrupted capture;
- then removes incomplete staging. An interrupted capture is never published.

If resuming the guest or persisting recovery fails, daemon startup fails and
keeps the operation journal for the next attempt. A failed restore also keeps
its journal when rollback cannot be persisted. Do not delete these records to
bypass an error; resolve the reported storage or VMM error and restart the
daemon.

This is ordered recovery, not atomicity. A restore interrupted after resume
but before the state was persisted is designed to come back running; one
interrupted earlier comes back stopped. In local acceptance, daemon kills at
`supervisor-started`, `loading` and `resuming` all rolled back to stopped. The
adopt-while-restoring branch is implemented but was not observed on real HVF.
Adoption of a completed restore after a daemon kill was observed.

## Network policy

The snapshot never widens network permissions. Egress, DNS forwarding and
listeners come from the daemon's current `--fc-security` policy. The fork
rejects snapshots whose `unix-stream` interface ID or MAC differ, and IP rules
inside the VMM configuration. A restored VM can reach only what a freshly
started VM with the same configuration could reach.

## Limits

- Not portable: the same host, macOS build, VMM build and device model are
  required, and the store is not an export format.
- No volumes, no slirp VMs, no cross-VM clone, no change of network or IP.
- Established TCP connections are lost across stop/restore.
- The guest is paused for the whole capture, and services do not answer
  during it. Jerboa does not adjust guest time. Guest clock behaviour across
  pause, capture and restore was not measured by the Jerboa acceptance.
- Snapshot sizes are roughly guest RAM plus the root disk. Quotas bound them,
  but there is no compression or deduplication.
- The Jerboa-owned `VM.Start` path does not use snapshots, and restart
  policies always boot the base image.
- Validation so far covers one Apple M5 host. See the acceptance report for
  what was tested; untested scenarios are not supported claims.

## Acceptance harnesses

The daemon/CLI acceptance runs an isolated `jerboad` (its own `HOME`,
`TMPDIR`, socket and dynamic ports) against real HVF guests:

```sh
export GO="$PWD/../firecracker-macos/experiments/hvf/build/guest-tools/go/bin/go"
B=path/to/jerboa-bundle/bin
python3 scripts/test-jerboa-snapshots.py --bin-dir path/to/jerboa-under-test \
  --firecracker-bin "$B/firecracker" --tools-dir "$B/tools" \
  --go-bin "$GO" --output-dir /absolute/output
```

It covers create and JSON-RPC restore, negative cases, races and daemon
`SIGKILL` recovery, and signals only processes carrying its private path.

The low-level fork harness drives the supervisor API directly with Jerboa's
switch, without `jerboad`:

```sh
python3 scripts/test-firecracker-snapshot-network.py \
  --firecracker-bin dist/sol-closure/snapshots/bin/firecracker \
  --seed-snapshot dist/sol-closure/snapshots/seed.snapshot \
  --output-dir dist/sol-closure/snapshots/versioned-run \
  --go-bin "$GO"
```

## Low-level VMM workflow

For reference, the fork API sequence the daemon automates:

```text
PUT /actions         {"action_type":"Pause"}
PUT /snapshot/create {"snapshot_path":"/absolute/private/vm.snapshot"}
GET /operations/<id>

# restore, from a supervisor started with only --api-sock
PUT /network-interfaces [{"iface_id":"eth0","guest_mac":"02:…","backend":"unix-stream","socket_path":"/absolute/private/new.sock"}]
PUT /security           {"version":1,"unix_stream":"/absolute/private/new.sock"}
PUT /snapshot/load      {"snapshot_path":"/absolute/private/vm.snapshot"}
GET /operations/<id>
PUT /actions            {"action_type":"Resume"}
```

Do not send `ForceStop` or snapshot requests directly to a Jerboa-managed VM's
supervisor. Use the CLI or API so the daemon's state, claims and journal stay
consistent.
