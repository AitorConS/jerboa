# Changelog

What changed in each version of Jerboa (the `jerboa` CLI, the `jerboad` daemon
and the Linux kernel toolset they ship with), written for people who use it.

The format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/). Each version
leads with the changes that can affect an existing setup and what to do about
them. Numbers such as (#275) are pull requests of
[AitorConS/jerboa](https://github.com/AitorConS/jerboa/pulls?q=is%3Apr+is%3Amerged).
Dates are the UTC day a version was published.

Which versions exist, and where:

- **Versions after 0.51.0** are published on the signed stable channel,
  `https://releases.jerboa.dev`. They have no git tag and no GitHub release.
  From 0.54.3 on, [`docs/releases/`](docs/releases/) holds the validation
  evidence of each one.
- **0.1.1 to 0.51.0** are the GitHub releases and tags of the same number.
  Their entries were reconstructed from the commits between tags, because
  those releases carry no notes. Until 0.47.0 the commands were called `uni`
  and `unid`.
- Version numbers skip wherever a number was assigned but never published.

## [Unreleased]

### Changed

- **`jerboa build` no longer lets a project relax its own build sandbox.** A
  `unikernel.toml` that sets `[build] sandbox = false` or
  `[build] network = true` makes the build fail before it runs anything. Read
  the project's `[build] run` commands and, if you trust them, pass
  `--allow-unsandboxed` or `--allow-build-network`. `examples/nextapp` needs
  `--allow-build-network`. (#275)
- **Runtime names without a namespace resolve only in `eyberg`.** `node:20` or
  `python:3.12` used to fall through to another publisher's package when
  `eyberg` lacked that version or architecture. Name the namespace to use
  another one, for example `--pkg nanovms/node:20`. (#275)
- **Compose state moves to `~/.jerboa/compose/`.** It used to sit next to the
  compose file. A state file found there is taken over only if `compose up`
  wrote it for that project and you own it; otherwise the command stops and
  names the file. (#276)
- **A compose project still up from 0.55.1 or earlier is refused.** Its state
  names its network and volumes without the project prefix that `compose up`
  has given them since 0.56.0, which is what a foreign state file looks like:
  `compose ps`, `down` and `up` stop and name the file. Take such a project
  down before upgrading, or afterwards remove its VMs, volumes and network
  with `jerboa rm`, `jerboa volume rm` and `jerboa network rm` and delete the
  `.jerboa-compose-*.json` file next to the compose file.
- **Compose service names are restricted** to letters, digits and `_.-`, the
  rule networks and volumes already follow. Rename services that use anything
  else. (#276)
- **Guest console output is filtered.** `jerboa run --attach`, `jerboa logs`
  and `jerboa compose logs` remove terminal control sequences from what a
  guest prints. `--raw` prints it as is. (#276)
- **The dashboard asks you to sign in once more after upgrading.** Its cookie
  is now a 12-hour session and no longer holds the observability token. (#277)
- **A VM whose running state cannot be saved does not start.** On a disk too
  full to record it, `jerboa run` fails instead of starting a VM the daemon
  would lose track of after a restart. (#266, #272)
- **A second `jerboad` on the same socket refuses to start**
  (`a daemon is already listening on …`). It used to take the socket over and
  leave the first daemon running and unreachable. A path that is not a socket
  is no longer deleted. (#273)
- **Package and release metadata older than what was already accepted is
  refused**, also on a machine with no saved state: the signed package index
  carries a serial, and the stable manifest a minimum generation time. (#281)

### Added

- **`jerboa doctor`** checks this machine, the daemon and what the daemon
  stores, and prints the command that fixes each problem. It changes nothing
  and exits non-zero when it finds one. `--deep` re-hashes every image disk.
  (#262)
- **`jerboa system df`** shows the space taken by images, VMs, volumes,
  snapshots, the build cache and the kernel tools, and how much of it nothing
  uses. (#262)
- **`jerboa system prune`** removes what nothing refers to: unnamed image
  disks, names of missing disks, orphan boot disks, unused build cache and
  leftovers of interrupted operations. `--vms`, `--all` and `--volumes` add
  stopped VMs, unused images and unmounted volumes. It lists what it will
  remove and asks first; `--dry-run` only lists. (#262)
- **`install.sh --uninstall`** removes a Linux or macOS installation: the
  service, the programs, the `jerboa` user and **all Jerboa data** (images,
  VMs, volumes, snapshots, credentials and settings). `--keep-data` removes
  only the programs and the service. On macOS, data is left alone while Jerboa
  Desktop is installed, since it uses the same data. (#260)

### Fixed

- **State survives power cuts, full disks and damaged files.** State files are
  written atomically and flushed. A damaged file no longer takes more down
  with it: an unreadable network lease file kept the daemon from starting, a
  damaged `refs.json` made every image disappear, and a disk copy cut short
  was recorded as the image. Stores now rebuild what they can and verify a
  disk before trusting it. (#259, #261)
- `jerboa images` lists the remaining images when one image's directory was
  deleted by hand, and the daemon drops such entries at start. (#259)
- A pull re-fetches a blob that went bad on disk instead of failing every
  time. (#267)
- With the SQLite VM store, migrated VMs are saved, and a removed VM no longer
  comes back at each daemon start. A VM removed before this version comes back
  one last time. (#263, #265)
- A CLI command no longer hangs forever on a daemon that accepted the
  connection and stopped answering: it ends with a hint to restart the daemon.
  Connecting and the handshake are bounded to 15 seconds. (#264, #273)
- When the daemon refuses a build, a volume seed or a volume import, the CLI
  reports the daemon's reason instead of `broken pipe`. (#268)
- Names inside a network: an alias shared by several running VMs is answered
  with all their addresses instead of being forwarded to the external
  resolver; the last VM of a full network no longer gets the broadcast
  address; the gateway's address can no longer be handed out or reserved with
  `--ip`; DNS answers over 512 bytes carry the truncation flag. (#263, #271)
- Builds resolving the same package for the first time at once no longer fail,
  and a package archive cut between two entries is reported instead of
  extracting with files missing. (#274)
- A VM removed during its restart backoff is not recreated, a VM whose network
  was removed can be removed, and an attached VM is reaped whether or not
  anyone reads its console. (#263, #283)
- Sizes that overflowed silently in `volume create`, `--memory-max` and
  `--disk-bps` are rejected. `volume ls` no longer panics on a null entry, and
  `registry pat create` no longer reports success with an empty token. (#263)
- **A Firecracker VM started again after a daemon restart has its network.**
  On Linux the tap device of a VM that was running when the daemon stopped
  (a restart, an upgrade) stayed behind, and the VM started in its place
  failed to set its network up and ran unreachable, with only a warning in
  the daemon log. The tap is now taken over.
- `jerboa compose down` also removes the copy of a project's state that an
  interrupted takeover left next to the compose file, which brought the
  project back after it was taken down; and a compose state file that does
  not parse is named in the error.

- **On Linux, VMs that keep running across a daemon restart are fully taken
  back.** A Firecracker VM whose daemon was killed or restarted outside
  systemd was recorded as stopped while its VMM went on running, with its
  volume open and its boot disk deleted under it; it is now re-adopted like a
  QEMU VM, and `jerboa stop` and `jerboa rm` work on it.
  The published ports of a re-adopted VM, which refused connections after
  the restart, answer again. (#293)
- `jerboa compose down --volumes` removes the stack's volume also after a
  `down` without `--volumes` and a second `up`. The second `up` used to stop
  recording the volume it reused, and nothing in compose removed it again.
  (#293)
- `jerboa volume rm` removes a volume whose disk is gone, which `jerboa
  doctor` tells you to do; it used to fail with `volume stat`. It still
  refuses while a VM mounts the volume. (#293)
- A `jerboa volume rm` interrupted by a daemon crash no longer leaves a volume
  that is listed but has no disk: the volume is gone, and
  `jerboa system prune` removes what is left of its files. (#293)
- On macOS, VMs start when `~/.jerboa` is on a filesystem that does not report
  holes in files (HFS+, some network mounts). `jerboa run` used to fail with
  `diskclone: seek data: inappropriate ioctl for device`. (#293)
- On Linux, an empty `JERBOA_DNS_UPSTREAM` turns off forwarding of guest DNS
  questions, as it does on macOS. It used to be read as unset, and questions
  went to `1.1.1.1`. (#293)

### Security

- `jerboa sign` and `jerboa verify` accepted an empty digest from the daemon,
  so an image without a digest verified. A digest containing a path separator
  could make the signature store read or write outside its directory. (#263)
- Confined build steps cannot reach host IPC brokers, and a build fails if a
  step changed `unikernel.toml` or `.unignore`. (#275)
- Terminal control sequences are stripped from untrusted text the CLI prints,
  such as names and messages that come from a registry or a guest. (#276)
- Attestations are bound to the repository being pulled and to its source.
  (#281)
- Guest DNS and the macOS shared network have per-VM budgets, so one guest
  cannot hold every DNS worker, forwarder attempt or relay slot. (#279, #280)
- Cluster gossip replies are bounded, a peer cannot rewrite other members by
  claiming their ID, and the target policy is enforced when dialing. (#278)
- The SQLite VM store is readable by its owner only, the VM runtime directory
  is never replaced by one that failed its privacy check, and the daemon's
  secret file is not written to the working directory when `HOME` is unset.
  (#277, #283)

## [0.59.0] - 2026-11-13

Kernel toolset 0.2.4, unchanged. Validation evidence:
[docs/releases/v0.59.0](https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.59.0).

### Changed

- **Firecracker is the default hypervisor wherever the machine can run it**,
  and QEMU otherwise: on Linux and inside WSL2 when KVM is available, the
  `firecracker` binary is installed and the daemon may create network devices;
  on Apple Silicon with macOS 26 or later. `jerboa daemon status` prints the
  hypervisor and what decided it. (#247, #249)
- **Existing installations keep QEMU until told otherwise.** Earlier releases
  wrote `hypervisor = "qemu"` into `~/.jerboa/config.toml`, which cannot be
  told apart from a choice. Run `jerboa config set hypervisor auto`, or answer
  the question Jerboa Desktop asks once. (#247)
- **Firecracker VMs on macOS reach the public Internet by default** (public
  IPv4 addresses and external names), as QEMU VMs do. The Mac itself and
  private, shared and link-local networks stay out of reach. To keep the old
  behavior, start the daemon with a policy file holding `{"version": 1}`:
  `JERBOA_FIRECRACKER_SECURITY=/path/policy.json jerboa daemon restart`. A
  policy you already use is unaffected. (#254)
- **Windows needs nested virtualization for Firecracker.** It is on by default
  on Windows 11; Jerboa Desktop and `jerboa daemon install` set it if it was
  turned off, and a running WSL2 VM keeps its setting until `wsl --shutdown`.
  Windows 10 stays on QEMU. (#247)
- A Linux daemon without `CAP_NET_ADMIN` picks QEMU and says why in its log.
  The systemd unit the installer writes grants the capability. Without KVM the
  installer installs QEMU instead of refusing. (#247)
- x86 images on macOS still need QEMU (`jerboa config set hypervisor qemu`).
- A question about a name of the network (an `AAAA` or `MX` record for a
  service, for instance) is answered locally and no longer forwarded to the
  external resolver. (#248)
- `jerboa push` uploads packs while it compresses the next ones, at most 24
  MiB at a time. (#251)

### Added

- `jerboa config set hypervisor auto` returns a machine to the automatic
  choice. (#247)
- Jerboa Desktop on macOS updates from inside the app, starting with the
  update after this one.

### Fixed

- `service.network` and `alias.network` resolve again on a compose network.
  They had stopped resolving in 0.56.0, when compose networks became scoped to
  their compose file. (#248)
- A snapshot restore interrupted by a daemon crash left one socket file behind
  after it was rolled back. (#248)

## [0.58.0] - 2026-11-12

Kernel toolset 0.2.4. Validation evidence:
[docs/releases/v0.58.0](https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.58.0).

### Changed

- **Which kernel an image runs.** With Firecracker every image boots the
  kernel of the installed release, so existing images get the faster kernel
  without a rebuild. With QEMU on Linux x86-64 an image boots the kernel it
  was built with: rebuild it to get the new one. Existing images and volumes
  need no change.
- **macOS VMs outside a network use an MTU of 9000** on their link, like
  Jerboa networks. Nothing to change in images. Through a published TCP port,
  iperf3 measured 9.7 Gbit/s into the guest (4.9 before) and 9.0 Gbit/s out of
  it (2.1 before). (#242)
- **A power cut right after a VM starts on macOS** may leave that VM's
  temporary VMM directory behind: start-up no longer flushes temporary files
  to the drive. No image, volume or VM state is involved. `jerboa run --rm` of
  a guest that prints one line takes 42 ms on an M5, down from 92 ms. (#238)
- `jerboa stop` under Firecracker on Linux shuts the guest down gracefully
  over vsock, as it already did on macOS and under QEMU. (#223)
- `jerboa pull` checks an image's signatures before it downloads the image,
  not after. Pulling an image whose disk is already stored rebuilds nothing.
  (#228, #229)
- Guest kernel networking is faster: TCP checksums are left to virtio devices
  that offload them, receive buffers are 16 KiB where the device merges
  buffers, and the device is notified once per pass. VM-to-VM TCP on Linux/KVM
  measured 2.5 Gbit/s, from 1.4, on a nested-KVM host. (#230, #234, #237)
- Guest kernel disk I/O: virtio-blk negotiates `EVENT_IDX`, and a sequential
  buffered writer starts writeback once it has dirtied 32 MiB. (#226, #227)
- The installer waits for `jerboad` to accept connections before it reports it
  running. (#224)
- Built with Go 1.27.4, which fixes GO-2026-6859. (#231)

### Added

- **Larger guests on macOS:** up to 8 vCPUs and 8192 MiB of memory, from 4 and
  2048. `--cpus` above 4 and `--memory` above 2G need the VMM of this release;
  an older one keeps the old limits and says so. (#240)
- **Registries behind Cloudflare Access:** `JERBOA_REGISTRY_ACCESS_HOST`,
  `JERBOA_REGISTRY_ACCESS_CLIENT_ID` and
  `JERBOA_REGISTRY_ACCESS_CLIENT_SECRET` send a service token to that registry
  only. (#235)
- Official image packages built from upstream release binaries, and Go
  programs built from source with a pinned toolchain. Packages are published
  separately from releases. (#222, #225)

### Fixed

- A registry read cut by the network is retried, a push whose 15-minute token
  expires asks for a new one and carries on, and the file that maps image
  names to digests is replaced atomically. (#239)
- `jerboa registry pat create` rejects an `--expires-in` out of range. (#221)

## [0.57.0] - 2026-11-08

Kernel toolset 0.2.3, unchanged. Validation evidence:
[docs/releases/v0.57.0](https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.57.0).

### Changed

- **Pulls verify signed images.** An image that has signatures is kept only if
  they verify, and the first verified pull remembers the signer: later pulls
  refuse another signer or an unsigned image. If a repository's publisher
  changes on purpose, run `jerboa registry signers forget <repo>`. `--verify`
  also refuses unsigned images. (#210)
- **`jerboa/` images must be signed by the official workflow.** An unsigned
  image, or one signed by anyone else, under `jerboa/` is refused. (#210)
- **Pushes from GitHub Actions sign by default** when they push with the job's
  OIDC token. `--sign=false` skips signing. (#210, #215)
- Clients up to 0.56.0 that pull a tag published for several platforms get the
  `linux/amd64` image. (#208)

### Added

- **Signed images.** A push from GitHub Actions by a trusted publisher is
  signed keyless with Sigstore and carries its SLSA provenance and its SBOM.
  `jerboa registry verify` checks an image without pulling it, and
  `jerboa registry signers` lists the remembered signers. (#210)
- **Official images** under `jerboa/`, built by
  [AitorConS/jerboa-images](https://github.com/AitorConS/jerboa-images),
  starting with `jerboa/redis` and `jerboa/nginx`.
- **Several platforms under one tag.** `jerboa push --index <target> <image>…`
  tags an image index of images already pushed; `jerboa pull` picks this
  host's platform, or the one `--platform` names. (#208)
- **SBOMs.** Every build records a CycloneDX SBOM. `jerboa images sbom <ref>`
  prints it and `jerboa build --sbom <file>` writes it to a file. (#209)
- **Publishing from CI without a stored token.** In a GitHub Actions job with
  `permissions: id-token: write`, `jerboa push` exchanges the job's OIDC token
  for a push token. The `setup-jerboa` action installs a pinned, verified
  release in a workflow. (#202)
- `jerboa registry repo rm ns/repo:tag` deletes a tag, and
  `jerboa registry repo rm ns/repo` a repository with all its tags, after
  asking. (#200)

### Security

- go-jose 4.1.6, which fixes a denial of service when parsing JWKs. (#217)

## [0.56.0] - 2026-10-30

Kernel toolset 0.2.3. Validation evidence:
[docs/releases/v0.56.0](https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.56.0).

### Changed

- **Restart the daemon after upgrading** (`jerboa daemon restart`). This CLI
  refuses a daemon from an earlier version. (#162, #185)
- **Observability and cluster tokens.** `jerboad` needs a token to serve the
  dashboard (on loopback too), metrics off loopback, or cluster gossip.
  `jerboa daemon start` sets one up. Otherwise use
  `--observability-token-file` / `--cluster-token-file`, or `--insecure` /
  `--insecure-cluster`. A token on a TCP endpoint must be at least 32
  characters. (#163, #188)
- **Image ports publish on `127.0.0.1`.** Ports from an image's `[run] ports`
  are no longer reachable from other hosts; use `-p` to publish elsewhere.
  (#187)
- **Networks.** A VM runs only on a network created with
  `jerboa network create`. Subnets must be private IPv4 ranges (/16 to /29)
  that do not overlap the host's addresses or routes. Guests are IPv4 only and
  cannot reach the host's loopback. (#165, #177, #180)
- **macOS QEMU guests reach public addresses only.**
  `JERBOA_GUEST_HOST_ACCESS=1 jerboa daemon restart` restores access to the
  Mac's localhost and private ranges. (#165)
- **Compose volumes and networks are scoped to their compose file.** After
  upgrading, `compose up` creates new, empty volumes; mark a volume
  `external: true` to keep using the old one. (#183)
- **Build steps run in a sandbox** (bubblewrap on Linux, `sandbox-exec` on
  macOS) and without Jerboa's tokens. (#172, #189)
  - `[build] run` commands have no network unless `[build] network = true`.
  - Steps that write outside the project, the caches and `TMPDIR` fail.
  - npm runs with `--ignore-scripts` unless `[build] install_scripts = true`.
  - Credential files (`.env*`, `.npmrc`, `*.pem`, `*.key`, SSH keys…) are left
    out of the build context; re-include one in `.unignore`.
  - The first confined build downloads dependencies again into Jerboa's own
    caches.
  - `[build] sandbox = false` turns confinement off.
- **Image signatures made by earlier versions no longer verify.** Run
  `jerboa sign` again.
- **Legacy client-side volumes** must be moved with `jerboa volume migrate`.
- **Installing an older version** with `install.sh --version` needs that
  version's signed release manifest. (#191)
- Images are reproducible and host-independent, and compact by default.
  `disk_size` is applied when the VM starts. (#136, #137, #141)
- Downloads resume after an interruption, and the kernel toolset is downloaded
  once, in parallel. (#138)

### Added

- **Registry.** `jerboa login`, `logout`, `tag`, `push` and `pull` work
  against the Jerboa registry (`registry.jerboa.dev`). `jerboa login` signs in
  with GitHub through the registry. `jerboa registry pat` manages personal
  access tokens, and `jerboa registry repo edit` sets a repository's
  description and README. (#145, #147, #156, #164, #192)
- Every build is recorded as a recipe (`jerboa images recipe`). (#144)
- Runtime packages are reproducible archives identified by a content digest.
  The package index is signed, and the client verifies it before use. (#139,
  #170)
- Kernel toolset 0.2.3: `mkfs` produces byte-identical images on every host,
  accepts host paths longer than 255 bytes and is published for every host
  platform. (#141, #143)
- Native macOS runtime: block requests run on worker threads, network frames
  keep backpressure, and links between VMs on the same Mac go direct.

### Deprecated

- The `--auth-token`, `--cluster-token` and `--observability-token` value
  flags, in favour of their `-file` variants.

### Security

- The daemon API uses a mutual challenge handshake, so the token never crosses
  the connection, and limits what an unauthenticated client can hold. (#162,
  #185, #186)
- VMMs run without the daemon's capabilities, in a private runtime directory,
  with QEMU's seccomp sandbox on Linux. (#167, #187)
- Release manifests older than one already accepted are refused, by the CLI
  and by `install.sh`. (#171, #191)
- Host file access through manifests and the daemon API is restricted, and
  registry responses are validated on push and pull. (#160, #161)
- On the native macOS runtime, guest-driven logs are bounded and oversized
  network frames are dropped instead of stopping the VM.

## [0.55.1] - 2026-10-18

Kernel toolset 0.2.2, unchanged. Validation evidence:
[docs/releases/v0.55.1](https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.55.1).

### Changed

- Ships the redesigned Jerboa Desktop. The engine is the same as 0.55.0 apart
  from documentation page descriptions. (#133)

## [0.55.0] - 2026-10-17

Kernel toolset 0.2.2. Validation evidence:
[docs/releases/v0.55.0](https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.55.0).
The Windows installer test did not run for this release (#130).

### Changed

- **Jerboa networks use an MTU of 9000** on macOS and Linux, and the guest
  kernel sizes its buffers and TCP windows for it. VM-to-VM TCP measured
  3.9–4.1 Gbit/s on macOS (from 2.1–2.3) and 1.0 Gbit/s on a nested-KVM Linux
  host (from 0.47). (#119, #120)
- **Linux installs Firecracker 1.17.0** instead of 1.10.1. It boots the kernel
  through PVH, so it needs the kernel of this release: `jerboa kernel update`
  now also refreshes the cached Firecracker kernel. (#125, #127)
- Firecracker drives on Linux use the async (io_uring) block engine when the
  host supports it, and 2 MiB huge pages when enough are free.
  `jerboa run --disk-io-engine sync` keeps the old engine. (#115)
- On Linux, published ports reached from the host itself go through kernel NAT
  instead of the userspace proxy, and UDP published ports work. (#121)
- On macOS, VMs on the same network talk over direct links, with larger socket
  buffers and whole TSO frames to the gateway. (#122, #123, #124, #129)
- New volumes use TFS format v6, in which a sequential first fill writes only
  the pages it touches. Existing volumes are not migrated and keep working.
  (#114)

### Added

- `jerboad --port-publish nat-lan` (Linux) also uses kernel NAT for published
  ports reached from other hosts, and opens nftables and firewalld policies
  for them. (#126, #128)

### Fixed

- TFS durability: storage released by a delete or truncate is reused only once
  the release is durable, and written blocks become readable only after their
  data is flushed. Fixes for volumes that fill up. (#113, #114)
- TFS no longer flushes the device on every delete or truncate, which made a
  PostgreSQL guest time out under WSL2. (#131)
- VMs left in `stopping` when the daemon exited are recovered when it starts
  again. (#121)

## [0.54.3] - 2026-09-24

Kernel toolset 0.2.1. Validation evidence:
[docs/releases/v0.54.3](https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.54.3).

### Changed

- **Update the CLI and the daemon together** (wire protocol 3). Existing Linux
  installations also need the updated service unit, which the installer
  writes. (#100)
- **Named volumes are owned by the daemon.** Volumes created by earlier
  clients are moved with `jerboa volume migrate`. (#100, #104)
- Rebuild guest images to get the kernel fixes below.

### Fixed

- Guest kernel: memory corruption when pages were discarded under load,
  stalled disk I/O, incomplete TCP transfers and a TFS failure after a file
  was shrunk and grown again. These were found with the 0.54.2 benchmark
  workloads on Linux and macOS. (#100)
- Empty arguments in an image's command line are preserved, glibc runtime
  libraries a program needs are included, and released IP leases are
  recovered. (#100)
- The x86 kernel no longer faults at boot under Firecracker 1.16. (#102)
- On Windows, `jerboa volume` commands reach the daemon in WSL2 instead of the
  default local endpoint. (#104)

## [0.54.2] - 2026-09-22

Kernel toolset 0.2.0. No validation evidence was recorded for this version;
this entry comes from its pull request.

### Changed

- **Volumes under Firecracker are durable.** Firecracker drives used a cache
  mode that silently dropped the guest's flush requests, so a database `fsync`
  on a volume was not durable. Volumes now default to `writeback`;
  `jerboa run --volume-cache unsafe` restores the old behavior. (#92)
- A VM starts from a clone of its image instead of a full copy. Start-up with
  a 1 GiB image measured 359 ms, from 635 ms. Boot disks live in
  `jerboad --vm-disk-dir` (default `~/.jerboa/vm-disks`). (#92)
- Images are smaller: the boot partition is sized from its contents. (#92)

### Added

- A compact image layout (`jerboa build --layout compact`, `[build] layout`)
  for hypervisors that load the kernel themselves. QEMU refuses compact images
  where it boots through BIOS. (#92)
- `jerboa run --disk-io-engine sync|async` and
  `--volume-cache writeback|unsafe`. (#92)
- `[build] include`, an allowlist for the build context, and
  `jerboa build --size-report[=json]`. (#92)

### Fixed

- Stopping a Firecracker VM on Linux no longer waits out the 30-second grace
  period. (#92)

## [0.54.1] - 2026-09-16

Kernel toolset 0.2.0. The first version with native macOS support. 0.52.0,
0.53.0 and 0.54.0 were assigned while it was being integrated and were never
published.

### Added

- **Native macOS on Apple Silicon.** Guests run through a Firecracker port to
  Hypervisor.framework (macOS 26 or later), with shared networks, scoped DNS,
  static addresses, TCP and UDP port publishing, an outbound policy, volumes,
  Compose, health checks and recovery after a daemon crash. QEMU remains
  available. (#82)
- **Snapshots** on macOS: create and restore in place. They are local
  development artifacts and refuse VMs with volumes attached. (#82)
- `install.sh` detects Linux x86-64 or macOS arm64, installs a signed release
  and starts the daemon (systemd on Linux, launchd on macOS). `--version`
  installs a pinned release. (#78, #82)
- A macOS user guide in the documentation. (#91)

### Fixed

- `jerboa build` on a macOS or Windows executable explains how to build a
  Linux binary instead of failing with `parse elf: bad magic number`. (#89)
- `jerboa stats` shows network counters for a macOS VM started without
  `--network`. (#89)
- Boot images are verified in one pass. (#78)

## [0.51.2] - 2026-09-06

Kernel toolset 0.2.0.

### Changed

- **Update the CLI and the daemon together** (wire protocol 2). (#73)
- `jerboa rmi` refuses to remove an image a VM still references. (#71)
- `jerboa stop` and `jerboa kill` on a VM that is already stopped do nothing
  instead of failing. (#71)
- `jerboa compose down` stops and removes the stack's VMs. Existing stacks
  follow the migration described in the Compose documentation. (#71, #73)
- A VM name already in use is refused, and a `run` whose port forwarder cannot
  start is aborted instead of leaving a VM with a dead port. (#61)
- `jerboa pkg list` without `--source` lists both package sources. (#71)

### Fixed

- Large downloads are no longer cut off after 30 seconds. (#61)
- Static addresses are validated and reserved, overlapping subnets are
  refused, and removing a network removes its bridge. (#61)
- `--restart on-failure` restarts a VM when QEMU itself fails, which was read
  as a clean guest exit. (#61, #65)
- `jerboa pkg from-docker` works on scratch and distroless images, and
  `volume seed`, `sign`, `verify` and `pkg load` reach the daemon on Windows.
  (#66, #71)
- `jerboa stats` reports real CPU, memory and network figures under
  Firecracker. (#66, #71)
- Image integrity is verified against the actual image contents, and replacing
  the WSL2 daemon exports a recovery backup first. (#73)

## [0.51.1] - 2026-08-10

Kernel toolset 0.2.0. The first version built for the signed stable channel.
Its files are no longer on the channel, so its publication could not be
confirmed; the date is the day the version was assigned (#59).

### Changed

- **Releases come from a signed channel.** The CLI, the daemon, the WSL2
  distro and the kernel toolset are downloaded from
  `https://releases.jerboa.dev`, described by a minisign-signed manifest, and
  each file is checked against the SHA-256 that manifest records. GitHub
  releases stop here. (#32, #34)
- **`jerboad` refuses to start on a TCP endpoint without a token** unless
  `--insecure` is passed. The Unix socket and the config file are readable by
  their owner only. (#24)
- The CLI and the daemon negotiate a protocol version and refuse a mismatch.
  (#34)
- `ops` is the default package source. (#23)

### Added

- `jerboa version` compares the installed CLI and kernel with the latest of
  every component. `jerboa kernel check` and `kernel update` follow the signed
  manifest. (#34)
- `jerboa daemon reinstall --keep-data` replaces the WSL2 distro and keeps
  images, VMs and networks. (#37, #39)
- **Volume seeding** for databases that need initial data
  (`jerboa volume seed`). (#14)
- A DNS server for guests on shared networks, and one TAP device per VM. (#16)
- Build: packages declared in `unikernel.toml` (`[build] pkgs`), checks before
  the image is assembled, hints for common guest boot errors, `jerboa init`
  and `jerboa build --smoke`. (#23)
- `jerboa pkg create --sysroot` resolves shared libraries against a root
  filesystem instead of the host, and warns about version mismatches. (#21)
- A VM that names a network without an address gets one from the daemon. (#26)
- Optional tokens for the dashboard, metrics and cluster gossip. (#28)

### Removed

- `jerboa service` and its manager. (#18)
- `jerboa kernel list` and `kernel use`, and the `jerboa self` update commands
  that existed between 0.51.0 and this version. (#34)

### Fixed

- Each Firecracker VM boots from a private copy of its root disk. (#20)
- VM lifecycle counters and the image count in the metrics reflect real
  events. (#19)

### Security

- Package archives cannot write outside their directory through symlinks or
  `..` paths, and package names, versions and namespaces from a remote index
  are validated before they become paths. (#28, #48, #53)
- Network and volume names are validated, and values that reach the
  Firecracker kernel command line cannot inject boot arguments. (#24)
- Cluster gossip input is validated. (#48)

## [0.51.0] - 2026-06-30

Kernel 0.2.0.

### Added

- `disk_size` in `unikernel.toml`, and a MySQL example. (#11)
- Persistent TFS volumes mounted when the VM starts.
- `[run]`, `[env]` and `dirs` in `unikernel.toml` are honored, and raw
  programs run from their real path. (#12)

### Fixed

- A successful build no longer ends with `broken pipe`. (#13)
- `jerboa run` uses default memory and CPU values when neither a flag nor the
  image gives them.

## [0.50.0] - 2026-06-29

### Changed

- Release files are checked with a `SHA256SUMS.txt` file instead of cosign
  signatures and an SBOM.

## [0.49.0] - 2026-06-29

### Changed

- **Networking is TAP only**, with a userspace port forwarder for published
  ports, under QEMU and Firecracker. Each VM gets a unique MAC address. (#3)
- `jerboa ps` shows the image reference instead of the disk path.

### Added

- A Windows installer.
- `jerboa logs -f` follows a VM's console.
- VM runtime hardening: bounded logs, a QMP socket, input validation and
  recovery after a daemon crash. (#2)

### Removed

- The self-update commands. Updates are manual from this version.

## [0.48.0] - 2026-06-26

The project, the commands and the binaries are renamed from `uni` / `unid` to
`jerboa` / `jerboad`.

### Changed

- **The CLI is a client of the daemon.** `build`, `run`, `images` and `rmi` go
  through it, over `unix://` or `tcp://` endpoints with token authentication.
  The daemon runs on Linux only.
- **On Windows the daemon runs inside a dedicated WSL2 distro** that starts on
  demand; `jerboa daemon` manages it.
- Image signatures are keyed by image digest and kept by the daemon.
- VMs are stopped through QMP instead of OS signals.

### Added

- **Firecracker** as a hypervisor on Linux and inside WSL2. (#1)
- A native Linux installer.
- `unikernel.toml`: `lang = "raw"` for prebuilt runtimes through `[program]`,
  `[build] run` for build steps, and environment variables carried into the
  image.
- `--verbose`, a progress spinner, and timing and size figures in `build` and
  `run`.
- Flask, Next.js and Ruby examples.

### Removed

- `uni cp`.

### Fixed

- Python builds download Linux wheels instead of the host's, and several
  package resolution errors with `ops` packages.

## [0.47.0] - 2026-05-28

### Security

- Kernel tool and upgrade downloads are checked against SHA-256 checksums.
- Package extraction rejects entries that escape the extraction directory.

### Changed

- The Windows upgrade replaces the binary by copying, and TLS keys are written
  in PKCS#8, to avoid antivirus false positives.

## [0.46.0] - 2026-05-27

### Added

- Packages from the nanovms/ops repository (`--source ops`).

## [0.45.0] - 2026-05-27

### Changed

- No user-facing change: tests for the package system only.

## [0.44.3] - 2026-05-27

### Removed

- The built-in registry: the registry server and the `push`, `pull` and
  registry commands of the CLI and the daemon. A registry returned in 0.56.0.

### Added

- `uni smoke`, a binary that checks the CLI's features against a daemon.

## [0.42.2] - 2026-05-25

### Fixed

- Builds on Windows pass manifest paths that `mkfs` inside WSL can read.

## [0.42.1] - 2026-05-17

### Added

- A health timeout for services, and `replicas` and an update strategy in
  compose files.

## [0.42.0] - 2026-05-17

### Added

- Service orchestration: `uni service`, rolling updates and round-robin DNS
  between replicas. It was removed again in 0.51.1.

## [0.41.1] - 2026-05-17

### Added

- `uni pkg from-docker` and `uni pkg push`, `--missing-files`, and multi-stage
  builds.

## [0.40.0] - 2026-05-16

### Added

- `uni pkg create` builds a package from a binary.

## [0.39.0] - 2026-05-16

### Changed

- No user-facing change: test coverage only.

## [0.38.0] - 2026-05-16

### Changed

- No user-facing change: documentation only.

## [0.37.3] - 2026-05-16

### Added

- Cluster membership between daemons (`unid --cluster-addr`, `--join`) and
  `uni node ls`.
- cgroup v2 resource quotas and disk I/O throttling for VMs.

## [0.33.1] - 2026-05-15

### Added

- `uni stats` shows live CPU, memory and network I/O per VM.
- A web dashboard (`unid --ui-addr`) with a page per VM, its logs and live
  statistics.
- A SQLite store for VM state (`unid --vm-store sqlite`), with migration from
  the file store. Health status survives a daemon restart.
- OpenTelemetry tracing (`unid --trace-addr`).

## [0.23.0] - 2026-05-12

### Added

- Structured JSON logs (`--log-format`).

## [0.22.0] - 2026-05-12

### Added

- A Prometheus metrics endpoint with VM state gauges and lifecycle counters.

## [0.21.1] - 2026-05-12

### Added

- `uni build --platform` builds for another architecture.

## [0.20.0] - 2026-05-12

### Added

- A build cache that skips a rebuild when the sources did not change.
- `.unignore` filters the build context.

## [0.19.1] - 2026-05-12

### Added

- Python and Rust builds.

## [0.19.0] - 2026-05-12

### Added

- Node builds: `npm install`, `engines.node` and entrypoint detection.

## [0.18.2] - 2026-05-12

### Added

- `unikernel.toml`, the project's build configuration file.

## [0.18.1] - 2026-05-12

### Added

- Language detection for Node, Python and Rust projects.

## [0.18.0] - 2026-05-12

### Added

- Build drivers: `uni build` compiles Go projects itself, and `--lang` picks
  the driver.

## [0.17.1] - 2026-05-12

### Added

- `unireg`, a standalone registry service, with chunked uploads, scoped
  authentication and a self-signed TLS certificate on first start.
- Ed25519 image signing and verification.

## [0.12.0] - 2026-05-11

### Added

- Registry: nested repository paths, TLS, blob garbage collection, a search
  command, and authentication and TLS options for `push` and `pull`.

## [0.6.0] - 2026-05-11

### Added

- Registry: scoped JWT authentication per repository action.

## [0.5.0] - 2026-05-11

### Added

- Registry: optional bearer authentication.

## [0.3.3] - 2026-05-10

### Changed

- `uni push` and `uni pull` use the OCI protocol by default.

### Added

- DNS commands and name resolution scoped to a network.

## [0.3.2] - 2026-05-08

### Added

- An internal DNS resolver for VMs.

## [0.3.1] - 2026-05-07

### Added

- Health checks and restart policies in compose files.

## [0.3.0] - 2026-05-07

### Added

- Networks: address management, `uni network` and compose integration.
- Health checks (TCP and HTTP probes), restart policies and `uni status`.
- VM state survives a daemon restart.
- Packages are checked against a SHA-256 on download.

## [0.2.0] - 2026-05-01

### Added

- Compose volumes, created on `up` and removed with `down --volumes`.
- Static guest addresses over bridge and TAP networking.
- `uni cp` copies files into a stopped VM.
- The package system: `uni pkg list`, `search`, `get` and `remove`, and
  `uni build --pkg`.

## [0.1.19] - 2026-04-29

### Fixed

- QEMU failed to start because the serial console was configured twice.

## [0.1.12] - 2026-04-29

### Fixed

- `uni upgrade` removes the backup files of earlier upgrades.

## [0.1.10] - 2026-04-29

### Changed

- `uni cp` downloads the tool it needs on first use.

## [0.1.9] - 2026-04-29

### Added

- `uni run --ip` and `--detach`, port forwarding over TAP, and `uni cp`.

## [0.1.8] - 2026-04-29

### Fixed

- `uni run --attach` failed before streaming any output.

## [0.1.7] - 2026-04-27

### Added

- `uni run --attach` streams the VM's console.

## [0.1.6] - 2026-04-26

### Fixed

- `uni upgrade` on Windows no longer fails when antivirus locks the downloaded
  file.

## [0.1.5] - 2026-04-26

### Added

- `uni upgrade` shows and checks the version of the running daemon.

## [0.1.4] - 2026-04-26

### Changed

- `uni upgrade` replaces the binaries atomically and stops and restarts the
  daemon.

## [0.1.3] - 2026-04-26

### Fixed

- `uni upgrade` downloads `unid` together with `uni`.

## [0.1.2] - 2026-04-26

### Changed

- VMs are addressed by name or by ID prefix, and IDs are 12 hexadecimal
  characters.

### Fixed

- `mkfs` is linked statically, so it runs on hosts with an older glibc.

## [0.1.1] - 2026-04-26

The first published release. A 0.1.0 tag exists from the same day, without a
release.

### Added

- `uni run` boots a unikernel image under QEMU through the `unid` daemon.
- `uni build`, `push` and `pull`, and an image store and registry.
- `uni ps`, `logs`, `stop`, `rm`, `inspect` and `exec`.
- Compose: `up`, `down`, `ps` and `logs` for several services.
- Port mapping, environment variables, volumes and named VMs.
- A kernel forked from Nanos, with environment variables injected at boot.
- Windows support through WSL2, and `uni upgrade`.

[Unreleased]: https://github.com/AitorConS/jerboa/commits/main
[0.59.0]: https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.59.0
[0.58.0]: https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.58.0
[0.57.0]: https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.57.0
[0.56.0]: https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.56.0
[0.55.1]: https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.55.1
[0.55.0]: https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.55.0
[0.54.3]: https://github.com/AitorConS/jerboa/tree/main/docs/releases/v0.54.3
[0.54.2]: https://github.com/AitorConS/jerboa/commit/39d22b45d21e43e31544573d5aa65acbffd3cf3d
[0.54.1]: https://github.com/AitorConS/jerboa/commit/89d8d2841594af97428f7802c398f9612cc5da38
[0.51.2]: https://github.com/AitorConS/jerboa/commit/0ea8613f6be310601d68ab186adf6ed45bed122d
[0.51.1]: https://github.com/AitorConS/jerboa/commit/d34dae9179247b0d592683ffbbdf786e52acb03c
[0.51.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.51.0
[0.50.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.50.0
[0.49.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.49.0
[0.48.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.48.0
[0.47.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.47.0
[0.46.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.46.0
[0.45.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.45.0
[0.44.3]: https://github.com/AitorConS/jerboa/releases/tag/v0.44.3
[0.42.2]: https://github.com/AitorConS/jerboa/releases/tag/v0.42.2
[0.42.1]: https://github.com/AitorConS/jerboa/releases/tag/v0.42.1
[0.42.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.42.0
[0.41.1]: https://github.com/AitorConS/jerboa/releases/tag/v0.41.1
[0.40.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.40.0
[0.39.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.39.0
[0.38.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.38.0
[0.37.3]: https://github.com/AitorConS/jerboa/releases/tag/v0.37.3
[0.33.1]: https://github.com/AitorConS/jerboa/releases/tag/v0.33.1
[0.23.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.23.0
[0.22.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.22.0
[0.21.1]: https://github.com/AitorConS/jerboa/releases/tag/v0.21.1
[0.20.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.20.0
[0.19.1]: https://github.com/AitorConS/jerboa/releases/tag/v0.19.1
[0.19.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.19.0
[0.18.2]: https://github.com/AitorConS/jerboa/releases/tag/v0.18.2
[0.18.1]: https://github.com/AitorConS/jerboa/releases/tag/v0.18.1
[0.18.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.18.0
[0.17.1]: https://github.com/AitorConS/jerboa/releases/tag/v0.17.1
[0.12.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.12.0
[0.6.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.6.0
[0.5.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.5.0
[0.3.3]: https://github.com/AitorConS/jerboa/releases/tag/v0.3.3
[0.3.2]: https://github.com/AitorConS/jerboa/releases/tag/v0.3.2
[0.3.1]: https://github.com/AitorConS/jerboa/releases/tag/v0.3.1
[0.3.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.3.0
[0.2.0]: https://github.com/AitorConS/jerboa/releases/tag/v0.2.0
[0.1.19]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.19
[0.1.12]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.12
[0.1.10]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.10
[0.1.9]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.9
[0.1.8]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.8
[0.1.7]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.7
[0.1.6]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.6
[0.1.5]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.5
[0.1.4]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.4
[0.1.3]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.3
[0.1.2]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.2
[0.1.1]: https://github.com/AitorConS/jerboa/releases/tag/v0.1.1
