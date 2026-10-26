---
layout: default
title: Release operations
description: "How jerboa releases are built, validated and published: candidates, native macOS evidence, promotion and the signed stable channel."
nav_order: 14
---

# Release operations

The engine repository owns product publication. Desktop builds packages; the website
consumes signed published metadata. A VERSION.md edit alone never publishes anything.

## Daily validation

`main.yml` is a small entry point with one required status, **CI required**. It calls
`validate.yml`; the final check fails on missing, failed or cancelled selected jobs.
Documentation-only PRs skip code validation. Kernel, distro, workflow and integration/E2E
test changes run the full validation suite even on PRs. Main pushes and release candidates run full
validation. Other PRs run tidy, lint, unit/race/coverage, vulnerability and release
protocol checks. Keep the `kvm` environment review for code that will run on owned
machines. Do not weaken that review to shorten the queue.

The kernel artifact is built once and consumed by E2E and distro packaging. Do not
reuse outputs across incompatible architecture/libc/toolchain combinations. `ci-timings`
records actual job execution durations. GitHub does not expose a reliable ready-for-
runner timestamp in that API; queue and approval latency are explicitly unknown, not
calculated by subtracting workflow creation time. Compare p50/p95 over a representative
sample before claiming the 3–5 minute PR target has been achieved.

Kernel host tests are required in full CI and also run nightly on hosted Linux: they do not use KVM. For diagnosis,
manual `nightly.yml` dispatch accepts `suite: kernel`; scheduled/default runs still
execute all suites. Native Mac validation remains manual and distribution remains
preview, as selected for this rollout.

## Prepare a candidate

1. Merge validated engine and Desktop changes. Choose exact 40-character Desktop
   and Firecracker commits. The runtime SHA must have passed the native runtime tests.
2. Update `VERSION.md` to a **new** version if the intended stable bytes differ from
   any already-published version. Merge that change; it does not publish.
3. Select the Linux kernel toolset explicitly:
   - For kernel changes, update `kernel/VERSION` to a new version and merge it.
     Choose `kernel_source: build` (the default) and provide that version with a
     `v` prefix. The workflow stages the same-run validated Linux kernel build;
     it never substitutes the currently published kernel.
   - For a release intentionally reusing the published Linux tools, choose
     `kernel_source: stable` and their exact published version. The signed stable
     manifest and every downloaded file are verified. Do not use this option
     when the release is meant to deliver Linux kernel fixes.
   Dispatch **Build release candidate** (`release.yml`) on `main`, providing the
   product version, Desktop SHA, Firecracker SHA, kernel source and kernel version.
4. Approve the KVM validation environment after reviewing the source revisions.
5. Wait for validation, the selected toolset's QEMU/KVM and Firecracker boot
   checks, Linux builds, Mac packaging, Windows packaging and the full Windows
   installer smoke. Windows consumes the current run's CLI artifact plus an
   explicit version/hash manifest; it never fetches stable to decide what to package.
   The installer smoke runs on a Windows machine started for this run only (see
   [Windows installer smoke](#windows-installer-smoke)); assembly waits for it.
6. Download `release-candidate` and `macos-app` from the run. The candidate includes a
   signed manifest, a signed inventory with source revisions, exact payload hashes,
   and a generated Windows update feed. Stable remains unchanged.
7. Run native Mac tests on the candidate app using `scripts/release/native-macos.sh`.
   Preserve the JSON result with the candidate run ID and app archive hash in a
   reviewable HTTPS report. A hosted Mac VM does not provide Hypervisor.framework.

Candidates currently retain artifacts for 30 days (platform intermediates use the
repository default unless overridden). Expired candidates must be rebuilt and
revalidated. Never pretend a rebuilt binary is the old candidate. R2 release artifacts
are retained; these workflows contain no automated deletion.

### Windows installer smoke

The smoke installs the signed `desktop-windows` installer silently, provisions the
WSL2 distro from the run's `distro-rootfs` artifact and boots example unikernels
through `scripts/windows-install-smoke.sh`. Hosted runners do not offer WSL2, so the
candidate workflow brings its own machine:

- `windows-runner-up` (GitHub-hosted Linux, `windows-runner` environment) registers
  a single-job [just-in-time runner](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/autoscaling-with-self-hosted-runners#using-just-in-time-runners)
  labelled `jerboa-run-<run id>-<attempt>`, starts one EC2 Windows instance from the
  prepared image and launch template, and waits until the runner is online. It
  starts once Windows packaging and the WSL rootfs are built.
- `windows-smoke` runs on that runner only. It has read-only repository access and
  no secrets.
- `windows-runner-down` always runs once an instance was started: it terminates the
  instance and removes the runner registration if the machine never took the job.
  The instance also stops itself after its job and at the latest 90 minutes after
  boot.

`scripts/release/windows-runner.sh` holds these steps. AWS access uses the job's
OIDC token and a role that may only start and terminate these instances; the runner
is registered through a GitHub App installed on this repository alone. The image,
launch template, role and App come from the `jerboa-infra` Windows runner setup,
which also fills the `windows-runner` environment (main only): variables
`WINDOWS_RUNNER_ROLE_ARN`, `WINDOWS_RUNNER_REGION`,
`WINDOWS_RUNNER_LAUNCH_TEMPLATE_ID`, `WINDOWS_RUNNER_APP_ID` and secret
`WINDOWS_RUNNER_APP_PRIVATE_KEY`.

The smoke is required: if the environment is not configured, `windows-runner-up`
fails with the list of missing settings and no candidate is assembled. To retry a
failed smoke, re-run `windows-runner-up` (which re-runs its dependents) rather than
only the failed jobs: a new machine is registered under the new attempt's label.
To run the smoke by hand, use a Windows machine with WSL2 and follow the steps of
`windows-smoke`.

## Promote

Dispatch **Promote release** (`promote.yml`) on main with the successful candidate run
ID and the URL of native Mac test evidence. The `release` environment must require
an owner review. The reviewer checks that the evidence identifies the exact run and
app hash (matching `inventory.native_macos_app.sha256`), and that installation/VM creation/stop completed successfully. This is an
explicit transitional human gate, not an automated claim of native validation.

The coordinator rejects a failed/non-main candidate, wrong workflow, mixed versions,
missing platforms, invalid signatures, changed bytes, an older version, or a previously
published version with another manifest. Production paths remain compatible with old
clients, including `desktop/jerboa-desktop-setup-X.Y.Z.exe`.

Promotion uploads immutable objects, records a signed receipt, verifies public artifact
downloads, then updates `channels/stable.json`, its detached signature, and
`desktop/latest.yml`. The final step dispatches `Jerboa_Docs/release-sync.yml`, which
checks signed published metadata, verifies both download URLs, preserves existing
editorial notes and commits generated metadata. Verify the website workflow and Vercel
status separately; successful dispatch means queued, not deployed.

Public documentation is maintained in `AitorConS/Jerboa_Public_Docs`, with its own
CI and scheduled/manual sync workflow. Product promotion does not dispatch a
documentation deployment. The old engine `Deploy Docs` workflow has been removed;
keep `docs/` as an input to the public documentation sync.

The Linux download toolset and WSL rootfs contain the same selected `mkfs`,
`dump`, `boot.img`, `kernel.img` and `kernel-fc.img` bytes. Assembly reads the
rootfs archive and verifies each embedded tool against the selected toolset.
The manifest's kernel version and the distro's kernel version must agree.
Native Mac continues to carry its source-built ARM64 kernel and native tools,
validated using the candidate app on a physical Mac. Linux x86_64 tools are never
substituted into the Mac app.

The signed inventory includes the selection mode, engine SHA, candidate run,
per-file hashes and QEMU/Firecracker boot evidence bound to those hashes. The
boot job verifies bytes before and after execution; assembly and promotion check
the same identity again. The complete rootfs and installers are also hashed.
Promotion uploads the candidate's existing bytes, including new kernel paths,
before updating stable. It neither builds a kernel nor resolves one from stable.

## Reattempts and partial failures

- Before channel promotion: stable stays at the previous version. Re-run the failed
  promotion using the original successful candidate. Identical objects are skipped.
- During the manifest/signature transition: existing clients can temporarily fail
  signature verification. This is fail-closed. Re-run promotion immediately; a
  same-version resume is accepted only for the identical signed manifest.
- After stable, before Windows feed/web: re-run promotion. It verifies/reuses the same
  immutable objects and completes the remaining pointers; it never rebuilds.
- Web only: re-run `release-sync.yml` with the current stable version. It rejects an
  old version and updates no installer binaries.
- Existing version with different bytes: do not overwrite. Cut a new patch version.

Multi-object R2 updates and a Vercel deployment are not a transaction. A future client
protocol can use a single reference to a signed release envelope; preserve compatibility
with older clients while migrating. Do not describe current promotion as atomic.

## Rollback and signing

The automated promoter intentionally rejects downgrades. For broken application
behavior, publish a corrected patch (possibly based on the previous source). Repointing
stable backwards does not automatically downgrade already-updated Electron clients.
An emergency rollback requires a reviewed client/downgrade policy and restoration of
the complete old manifest/signature/feed set; never modify historic binary contents.

Every assembled manifest carries `generated`, its UTC creation time. The promoter
refuses to publish a manifest generated before the live one, and clients record, per
release origin and channel, the newest `generated` they accepted (in
`~/.jerboa/release-state.json`) and refuse an older manifest, or one without the
field once a dated one was seen. A restored old manifest is therefore refused by
clients that saw a newer one: an emergency rollback means re-signing the old content
as a new candidate, not republishing the old manifest bytes.

Mac continues to use ad-hoc signatures and the documented preview installation flow.
Production Developer ID/notarization requires a certificate and Apple credentials;
`sign-notarize-macos.sh` contains the runtime signing procedure. Windows uses
`CSC_LINK`/`CSC_KEY_PASSWORD` when configured. Configure and test these identities before
claiming signed/notarized distribution. Do not remove the Firecracker hypervisor
entitlement when signing nested binaries. Native-runner provisioning and commercial
signing credentials are external prerequisites, not fabricated by CI.

## Kernel and package releases

`kernel-release.yml` remains a standalone build diagnostic and does not publish.
Product releases no longer need a separate kernel publication: `release.yml`
selects the toolset, boots it on QEMU/KVM and Firecracker, packages it in the
candidate and signs its hashes. `promote.yml` publishes product and kernel together
through the existing immutable-object protocol.

For the first release containing the benchmark/kernel corrections, use
`kernel_source: build` and `kernel_version: v0.2.1` (matching `kernel/VERSION`).
No product version or stable pointer is advanced just by merging this workflow.
For later source builds, choose a new kernel version whenever the bytes differ
from a published toolset; source revisions embedded in the kernel can also change
bytes after engine-only edits. Use explicit `stable` reuse for CLI-only releases
when no new Linux kernel is intended. Reusing a kernel version with different
bytes, or promoting a lower kernel version, is rejected before changing stable.
Retries with identical published bytes remain safe.

A source-built kernel also publishes its host tools: `kernel/<v>/mkfs-linux-arm64`
(built on `ubuntu-24.04-arm` by `kernel-tools-arm64`), `kernel/<v>/mkfs-darwin-arm64`
(from `build-macos`) and `kernel/<v>/toolset.json`, signed with the release key,
which lists the mkfs for every host including the kernel's own `mkfs-linux-amd64`.
Image recipes name a toolset version, and clients assembling one fetch that
version's mkfs through this document (`tools.MkfsForVersion`). Assembly checks the
binaries' headers match their platforms. A reused kernel already shipped these
files with its first release.

The `kvm` runner needs `cc` with static libc support, QEMU x86_64, Firecracker and
read/write access to `/dev/kvm`. The boot job uses isolated disks, records both
hypervisor versions and console-log hashes, and fails on timeout, non-successful
exit or missing guest output. It uploads `kernel-boot-logs` and only uploads
`validated-kernel` after both boots succeed. Missing or expired artifacts require
rebuilding/revalidating the candidate; they never trigger a download fallback.
Native Mac validation and owner approval of `release` remain required.

`packages.yml` runs independently on changed recipes or a manual package selection.
Builds use isolated hosted Linux machines and `fail-fast: false`. Package failures do
not turn a successful product promotion red. Pushes validate recipes without publishing. The index is updated only when selected
builds succeed on a manual run with `publish: true`. Existing package-index storage is retained; migrating package archives
to immutable per-recipe revisions is a separate format migration, not a product-release
side effect.

## Migration checklist

- Install the Desktop staging script and website sync workflow first.
- Validate the new engine workflow on its working branch with no stable promotion.
- Switch branch protection to **CI required** only after observing it pass; preserve
  strict/up-to-date protection and the KVM environment review.
- Configure the **release** environment with owner review and main-only deployment.
- Merge the new engine workflows; the previous main publisher and standalone Desktop
  publisher must both be disabled. Confirm there are no other stable writers.
- Build a candidate on main, validate native Mac evidence and review the inventory.
- Promote only a new release when it is intended for users. Implementing this migration
  does not itself require shipping a new application version.

### Bootstrap installer provenance

Candidate assembly includes `scripts/install.sh` as
`releases/<version>/install.sh` in the signed inventory. After promotion verifies
the release, it updates `https://releases.jerboa.dev/install.sh` from those exact
candidate bytes with `Cache-Control: no-cache`, then verifies the public download.
The `jerboa.dev/install.sh` redirect therefore receives installer fixes alongside
the release, including the daemon service capabilities needed for guest DNS.
Promotion never substitutes the installer from its own checkout. Retrying a
legacy candidate without a tracked installer leaves the bootstrap alias alone;
publish a new candidate to update it.

## Image registry operations

The image registry (`registry.jerboa.dev`, [user guide]({% link registry.md %}),
[protocol]({% link registry-protocol.md %})) server and its operations live in
a private repository. The Go client and the protocol stay here; the server's
CI runs this repository's `tests/registry` end to end on every change and
daily against `main`, so a client change that breaks the protocol fails there
within a day.
