# Jerboa v0.56.0

Release notes and validation evidence.

Candidate: [40483960462](https://github.com/AitorConS/jerboa/actions/runs/40483960462).

- Engine: `fb7ebe23a4adfd13065fe2277852e93ccee1e2ac`
- Desktop: `ac39d202a62f9f0a6ea60c2c4d18cf13c796edab`
- Firecracker macOS: `d1fe2641a391e43672e9fa6c07b335f5a5eee07c`
- Product: `v0.56.0`; source-built Linux kernel toolset: `v0.2.3`.

## What changes

**Registry.** `jerboa login`, `logout`, `tag`, `push` and `pull` work against the Jerboa registry (`registry.jerboa.dev`) and its hub. `jerboa login` signs in with GitHub through the registry, which runs the device flow itself. `jerboa registry pat` manages personal access tokens, with an expiry, namespaces and an optional delete permission. `jerboa registry repo` edits and deletes repositories.

**Images and packages.**
- Images are reproducible and host-independent, and are built compact by default. `disk_size` is applied when the VM starts.
- Every build is recorded as a recipe (`jerboa images recipe`).
- Runtime packages are reproducible archives identified by a content digest. The package index is signed, and the client verifies it before use.
- Downloads resume after an interruption, and the kernel toolset is downloaded once, in parallel.

**Linux kernel toolset v0.2.3.** mkfs produces byte-identical images on every host and accepts host paths longer than 255 bytes. mkfs is published for every host platform.

**Native macOS runtime.** Faster block and network I/O: block requests run on worker threads, network frames keep backpressure, and links between VMs on the same Mac go direct. Guest-driven logs are bounded, and oversized network frames are dropped instead of stopping the VM.

**Security.** Most of this release hardens the daemon, the network and the build:
- the daemon API uses a mutual challenge handshake, so the token never crosses the connection, and limits what an unauthenticated client can hold;
- build steps run in a sandbox (bubblewrap on Linux, `sandbox-exec` on macOS) and without Jerboa's tokens;
- VMMs run without the daemon's capabilities, in a private runtime directory, with QEMU's seccomp sandbox on Linux;
- guests are confined to Jerboa-managed networks, IPv4 only, and cannot reach the host's loopback;
- release manifests older than one already accepted are refused, by the CLI and by `install.sh`.

## Upgrade notes

These changes can affect existing setups:

- **Observability and cluster tokens.** `jerboad` needs a token to serve the dashboard (on loopback too), metrics off loopback, or cluster gossip. `jerboa daemon start` sets one up. Otherwise use `--observability-token-file` / `--cluster-token-file`, or `--insecure` / `--insecure-cluster`.
- **Tokens on the command line.** The `--auth-token`, `--cluster-token` and `--observability-token` value flags are deprecated in favour of the `-file` variants. A token on a TCP endpoint must be at least 32 characters.
- **Client and daemon versions.** This CLI refuses a daemon from an earlier version. Restart the daemon after upgrading (`jerboa daemon restart`).
- **Image ports.** Ports from an image's `[run] ports` publish on `127.0.0.1`. Use `-p` to publish elsewhere.
- **macOS QEMU guests** reach public addresses only. `JERBOA_GUEST_HOST_ACCESS=1 jerboa daemon restart` restores access to the Mac's localhost and private ranges.
- **Networks.** A VM runs only on a network created with `jerboa network create`. Subnets must be private IPv4 ranges (/16 to /29) that do not overlap the host's addresses or routes.
- **Compose.** Volumes and networks are scoped to their compose file. After upgrading, `compose up` creates new, empty volumes; mark a volume `external: true` to keep using the old one.
- **Builds.**
  - `[build] run` commands have no network unless `[build] network = true`.
  - Steps that write outside the project, the caches and `TMPDIR` fail.
  - npm runs with `--ignore-scripts` unless `[build] install_scripts = true`.
  - Credential files (`.env*`, `.npmrc`, `*.pem`, `*.key`, SSH keys…) are left out of the build context; re-include one in `.unignore`.
  - The first confined build downloads dependencies again into Jerboa's own caches.
  - `[build] sandbox = false` turns confinement off.
- **Image signatures.** Signatures made by earlier versions no longer verify. Run `jerboa sign` again.
- **Volumes.** Legacy client-side volumes must be moved with `jerboa volume migrate`.
- **Installing older versions.** `install.sh --version` needs that version's signed release manifest.

## Native Mac

The actual candidate app ZIP passed `scripts/release/native-macos.sh` on a physical Apple Silicon Mac running macOS 26.6.2. The checkout matched the candidate engine commit. The test used an isolated daemon and temporary VM state.

App: `jerboa-desktop-0.56.0-macos-arm64.zip`.

SHA-256: `cd48a3111e2af8a6bc98a5b29ec73147fd515c196eb41ed43e20f2748c4eefae` (matches `native_macos_app.sha256` in the candidate's signed inventory).

[Machine-readable result](native-macos.json) and [test output](native-macos.log).

Checks passed: app code-signature integrity, CLI/daemon versions, VM boot with 1 and 4 vCPUs, HTTP/UDP port forwarding with an omitted, loopback and wildcard bind, environment variables, persistent-volume contents across boots, port conflicts, daemon recovery, guest exit, restart after guest failure, and forced stop. These are functional checks, not throughput benchmarks.

## Linux kernel

The candidate's exact staged toolset passed the QEMU/KVM 8.2.2 and Firecracker 1.16.0 boot checks. The [kernel report](kernel-boot.json) is the candidate inventory's `kernel_boot` record, binding both results to the engine commit, candidate run and SHA-256 of each toolset file. The same files are embedded in the WSL rootfs and staged for release downloads.

## Windows

The candidate's signed Windows installer passed the full installer smoke on a Windows machine started for this run only: silent install, WSL2 distro provisioning from the run's rootfs, and the example unikernels.

## Publication gate

This directory records validation, not proof of publication. Promotion additionally requires the complete candidate workflow to succeed and the signed inventory to match this native app hash. Published versions are determined by the signed stable manifest at `https://releases.jerboa.dev/channels/stable.json`.
