---
layout: default
title: Release operations
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

Nightly kernel host tests run on hosted Linux: they do not use KVM. For diagnosis,
manual `nightly.yml` dispatch accepts `suite: kernel`; scheduled/default runs still
execute all suites. Native Mac validation remains manual and distribution remains
preview, as selected for this rollout.

## Prepare a candidate

1. Merge validated engine and Desktop changes. Choose exact 40-character Desktop
   and Firecracker commits. The runtime SHA must have passed the native runtime tests.
2. Update `VERSION.md` to a **new** version if the intended stable bytes differ from
   any already-published version. Merge that change; it does not publish.
3. Dispatch **Build release candidate** (`release.yml`) on `main`, providing that
   version, Desktop SHA, Firecracker SHA and the existing pinned kernel version.
4. Approve the KVM validation environment after reviewing the source revisions.
5. Wait for validation, Linux builds, Mac packaging, Windows packaging and the full
   Windows installer smoke. Windows consumes the current run's CLI artifact plus an
   explicit version/hash manifest; it never fetches stable to decide what to package.
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

The rootfs contains the source-built QEMU kernel validated in the candidate; the
manifest's kernel component is the separately pinned download toolset (including the
existing Firecracker kernel). Native Mac carries its source-built ARM64 kernel. The
signed inventory hashes the complete rootfs and installers, so these inputs are not
silently inferred from the current channel on a retry.

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

Mac continues to use ad-hoc signatures and the documented preview installation flow.
Production Developer ID/notarization requires a certificate and Apple credentials;
`sign-notarize-macos.sh` contains the runtime signing procedure. Windows uses
`CSC_LINK`/`CSC_KEY_PASSWORD` when configured. Configure and test these identities before
claiming signed/notarized distribution. Do not remove the Firecracker hypervisor
entitlement when signing nested binaries. Native-runner provisioning and commercial
signing credentials are external prerequisites, not fabricated by CI.

## Kernel and package releases

`kernel-release.yml` builds/validates candidate toolsets; it does not publish. A new
kernel requires QEMU and Firecracker boot validation and a new kernel version before
being made available to product candidates. The current coordinator carries forward
only the explicitly pinned, signed stable kernel. It refuses an implicit kernel change.

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
