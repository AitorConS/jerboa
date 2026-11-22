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

### Self-hosted KVM runner access

The `if:` guards (head repository must be this one) and `environment: kvm` on the
KVM jobs are lines of workflow files that a `pull_request` run takes from the pull
request itself: a fork can delete them, or add a workflow naming
`[self-hosted, linux, kvm]`. They document intent; they do not enforce it. What
enforces it is outside the pull request's reach:

- **Runner job-started hook.** Install `scripts/release/kvm-runner-guard.py` and
  its launcher `kvm-runner-guard.sh` on the runner, with the launcher as the
  `ACTIONS_RUNNER_HOOK_JOB_STARTED` script (the runner accepts only `.sh`, `.ps1`
  and `.js` hooks) and `JERBOA_KVM_REPOSITORY=AitorConS/jerboa` (see the
  script's header). Before any step
  of a job it refuses pull requests whose head is another repository, events other
  than push, pull_request, workflow_dispatch and schedule, and workflows other than
  `main.yml`, `release.yml` and `nightly.yml`. Push, `workflow_dispatch` and
  schedule jobs run only when both the ref and the workflow file are
  `refs/heads/main`: they wait for no review (the nightly KVM jobs carry no
  `environment: kvm`, so the scheduled run never waits for a person), and a
  dispatch can name any branch. Test a branch on the runner through its pull
  request, whose jobs wait for the `kvm` review. The check only takes effect
  once the guard is reinstalled on the runner. Reinstall it from main when it
  changes: the candidate's `kernel-test` job compares the installed copy with the
  revision being released (`kvm-runner-guard.py check-installed`) and fails with
  the install command when they differ. A unit test fails when a workflow with
  KVM jobs is missing from its list.
- **Settings > Actions > General:** "Require approval for all external
  contributors" for fork pull request workflows.
- **Environment `kvm`:** required reviewer (the owner), so same-repository pull
  request jobs wait for a review of the revision they run.
- **Runner:** dedicated to this repository, not shared with other repositories;
  preferably ephemeral (`--ephemeral`, re-registered per job). While it is
  persistent, a job can leave state for the next one, which is why release
  assembly trusts no artifact a KVM job could replace (see
  [Promote](#promote)).

Kernel host tests are required in full CI and also run nightly on hosted Linux: they do not use KVM. For diagnosis,
manual `nightly.yml` dispatch accepts `suite: kernel`; scheduled/default runs still
execute all suites. Native Mac validation remains manual and distribution remains
preview, as selected for this rollout.

### Upgrade tests

Two layers show that an installed machine survives the next release.

**State compatibility, with the unit tests.** `internal/upgradetest/testdata`
holds what released versions left on disk: `scripts/upgrade/make-fixture.py`
runs the released `jerboa` and `jerboad` (downloaded and checked against that
release's signed manifest) through a fixed scenario and packs their home
directories together with what that version reported about them. The tests in
`internal/upgradetest`, `internal/release` and `cmd/jerboad` restore that state
and require the current stores, daemon and CLI to show the same images, names,
VMs, volumes, networks, snapshots, compose project, credentials and settings,
also after a first start interrupted at every write, a half-finished migration
and damaged files. A fixture is restored where it was recorded
(`/var/tmp/jerboa-upgrade-fixture`), because the state names absolute paths.

Record a fixture for every release, once it is published:

```sh
scripts/upgrade/make-fixture-docker.sh v0.60.0     # Linux, in a container
python3 scripts/upgrade/make-fixture.py --version v0.60.0 \
    --bin-dir ".../Jerboa Desktop.app/Contents/Resources/bin" \
    --out internal/upgradetest/testdata             # macOS 26, natively
```

Keep the older ones: each is an upgrade path. `TestFixturesCoverTheLatestRelease`
fails once `VERSION.md` is more than one minor version ahead of the newest
Linux fixture, so the version bump of the release after a forgotten one does
not pass CI.

**A real upgrade, on the KVM runner.** The `Upgrade from stable` job of
`validate.yml` (full validation: main, release candidates, and pull requests
that touch the installer, the workflows or `tests/upgrade/`) boots a disposable
virtual machine and, inside it, installs the current stable release with the
public installer, creates an image, a VM with a volume and a compose project,
upgrades to the build under test with that build's `scripts/install.sh`,
checks that everything is still there and works, then goes back to stable and
forward again (`tests/upgrade/guest.sh`). The build under test is served from a
release origin made inside the machine, signed with a key generated for the
run; the installer is used unchanged except for the key it trusts. Nothing is
installed on the runner: `tests/upgrade/run.py` only writes inside its `--work`
directory. A release candidate cannot be built while this job fails.

## Prepare a candidate

1. Merge validated engine and Desktop changes. Choose exact 40-character Desktop
   and Spark commits. The candidate refuses a `runtime_sha` that is not on
   the main branch of `AitorConS/Spark` or whose check runs did not
   all succeed.
2. Run `python3 scripts/release/prepare-release.py X.Y.Z` and merge the result.
   It names the `Unreleased` section of `CHANGELOG.md` after the version (dated
   today in UTC, or `--date`), opens a new empty `Unreleased`, adds the version's
   link definition and writes `VERSION.md`. Use a **new** version whenever the
   intended stable bytes differ from an already-published version. Merging does
   not publish. The changelog entry is the release's notes: the candidate refuses
   a version without one, naming this command, and carries the entry as
   `releases/<v>/notes.md` in its signed inventory.
3. Select the Linux kernel toolset explicitly:
   - For kernel changes, update `kernel/VERSION` to a new version and merge it.
     Choose `kernel_source: build` (the default) and provide that version with a
     `v` prefix. The workflow stages the same-run validated Linux kernel build;
     it never substitutes the currently published kernel. `kernel-stage` fails,
     before any boot test or packaging, when that version is already published
     with other bytes or is older than the published one.
   - For a release intentionally reusing the published Linux tools, choose
     `kernel_source: stable` and their exact published version. The signed stable
     manifest and every downloaded file are verified. Do not use this option
     when the release is meant to deliver Linux kernel fixes.
   Dispatch **Build release candidate** (`release.yml`) on `main`, providing the
   product version, Desktop SHA, Spark SHA, kernel source and kernel version.
4. Approve the KVM validation environment after reviewing the source revisions.
5. Wait for validation, the selected toolset's QEMU/KVM and Firecracker boot
   checks, Linux builds, Mac packaging, Windows packaging and the full Windows
   installer smoke. Windows consumes the current run's CLI artifact plus an
   explicit version/hash manifest; it never fetches stable to decide what to package.
   The installer smoke runs on a Windows machine started for this run only (see
   [Windows installer smoke](#windows-installer-smoke)); assembly waits for it.
6. Download `release-candidate` and `macos-app` from the run. The candidate includes a
   manifest signed for the `candidate` channel, a signed inventory with source
   revisions, exact payload hashes, and a generated Windows update feed. Stable
   remains unchanged: clients and `install.sh` only accept a manifest whose channel
   is `stable`, and only promotion signs one, so an unpromoted or rejected candidate
   cannot be served to them as a release.
7. Run native Mac tests on the candidate app using `scripts/release/native-macos.sh`.
   Commit the JSON result as `docs/releases/<version>/native-macos.json` on main
   (with its log and the record of the release, which links to the changelog
   entry instead of repeating the notes). Promotion reads it from there. A hosted
   Mac VM does not provide Hypervisor.framework.

### What the candidate decides by itself

Nothing below is entered or remembered by the operator; the `inputs` job
(`candidate.py lock`) does it and the later jobs enforce it.

- **Stable floor.** `lock` downloads the live stable manifest, verifies its
  signature and records its `generated` time as `min_stable_generated`. Every
  `jerboa` and `jerboad` of the candidate is linked with
  `-X …/internal/release.MinStableGenerated=<that time>`, so a released client
  with no state of its own refuses any stable manifest older than the one that
  was live when its release was built. Assembly looks for the value in each CLI
  and daemon binary of each platform and refuses one that lacks it; the signed
  inventory records it. `lock` fails if the value committed in
  `internal/release/freshness.go` (the fallback of builds from source) is later
  than the live manifest, which would make source builds refuse stable: lower it
  in a pull request and build again.
- **Pipeline generation.** The inventory records `candidate.PIPELINE`. Promotion
  accepts only candidates of its own generation, so one assembled before a
  pipeline change is rebuilt instead of being promoted by mistake. Whoever
  changes what a candidate must contain bumps the number; a test pins the
  inventory's fields to it and fails when they change without a bump.
- **Release notes.** The changelog entry of the version (see step 2).
- **Runner guard.** See [Self-hosted KVM runner access](#self-hosted-kvm-runner-access).
- A version that is not newer than the published one only gets a warning: such a
  candidate can be built to exercise the pipeline, and promotion refuses it.

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
an owner review. Promotion itself checks that
`docs/releases/<version>/native-macos.json` in its checkout names this candidate
run, the hash of its app archive (`inventory.native_macos_app.sha256`), its
version and `"result": "pass"`, and refuses to publish otherwise; the reviewer
judges what the report says was tested. Running the native tests remains a
manual step, not an automated claim of native validation.

The coordinator rejects a failed/non-main candidate, wrong workflow, mixed versions,
missing platforms, invalid signatures, changed bytes, an older version, or a previously
published version with another manifest. Production paths remain compatible with old
clients, including `desktop/jerboa-desktop-setup-X.Y.Z.exe`.

It also rejects, before writing anything and with a message that says to
rebuild the candidate (dispatch **Build release candidate** again on main, repeat
the native Mac validation on the new run, promote that run):

- a candidate of another pipeline generation, or without a recorded stable floor
  or release notes;
- a candidate whose stable floor is older than the live stable manifest, that is,
  one built before the release that is now live was promoted: its binaries would
  accept a replay of the manifests that release replaced. A retry of the same
  candidate after a partial promotion is exempt.

Promotion signs the stable manifest only after its checks pass: the candidate's
manifest with its channel changed to `stable` and nothing else (the generation time
stays the candidate's, so a retry re-creates the same bytes and signature). It
uploads immutable objects, including that manifest as `releases/<v>/manifest.json`
and the candidate's as `releases/<v>/candidate-manifest.json`, records a signed
receipt, verifies public artifact downloads, then updates `channels/stable.json`,
its detached signature, and `desktop/latest.yml`. The release notes are published as
`releases/<v>/notes.md`. Candidates assembled before this
change carry a stable signature of their own and are refused; rebuild them. The final step dispatches `Jerboa_Docs/release-sync.yml`, which
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
validated using the candidate app on a physical Mac. Linux x86_64 host tools are
never substituted into the Mac app. Its x86 guest boot files (`tools/x86`, used only
for explicitly emulated linux/amd64 images) are the candidate's selected
`boot.img` and `kernel.img`, staged by `build-macos` from the verified
`selected-kernel` artifact; the job never fetches them from the live stable channel.

The signed inventory includes the selection mode, engine SHA, candidate run,
per-file hashes and QEMU/Firecracker boot evidence bound to those hashes. The
boot job verifies bytes before and after execution; assembly and promotion check
the same identity again. The complete rootfs and installers are also hashed.
Promotion uploads the candidate's existing bytes, including new kernel paths,
before updating stable. It neither builds a kernel nor resolves one from stable.

Artifacts of a run share one namespace, which the run's self-hosted KVM jobs can
also write to, and are downloaded by name. Each GitHub-hosted job that uploads an
artifact a later trusted job uses therefore records its digest
(`scripts/release/artifact-digest.py record`) as a job output, which no other job
can set, and the consumer checks the download against it before using, executing
or signing anything (`verify`; `validate.yml` exports `kernel_linux_sha256` for
`kernel-linux`). Assembly downloads every artifact by its own name, never by
pattern. The KVM runner's `validated-kernel` is not trusted on its own: it must
equal the verified `selected-kernel` apart from its boot report.

Every workflow downloads each artifact into a directory of its own outside the
checkout, `${{ runner.temp }}/artifacts/<name>`, never over the checkout: an
artifact unpacked there could replace `scripts/release/` before the scripts
that check it and sign the release run. The download is verified where it
lands, and only then `artifact-digest.py copy` puts its files where the
scripts read them (`release-lock.json`, `kernel-selected/`, `dist/`, ...).
`copy` refuses symlinks and special files, never overwrites a file, never
writes outside the working directory or into what the job runs or builds from
(`scripts/`, `.github/`, `cmd/`, `internal/`, `kernel/`, `go.mod`, `go.sum`,
`Makefile`, `bin/`, ...), and, given file names, refuses an artifact that holds
anything else. The kernel archive is unpacked from its download directory and
only its `kernel/output` is extracted. `test_workflows.py` fails on a
download step whose `path` is anything else. A missing or
different digest fails the job; rebuild the candidate rather than re-running only
the consumer. The release binaries assembly runs for their version check get an
environment without the signing seed or other credentials.

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

Clients with no state of their own (a first run, or `~/.jerboa` lost) still have a
floor for the stable channel: `release.MinStableGenerated`, compiled into the binary,
and the generation time `install.sh` recorded in `/etc/jerboa/release-state.json`,
whichever is newer. A stable manifest older than that floor, or without
`generated`, is refused. Release binaries get the floor from the candidate
workflow: the generation time of the stable manifest that was live when the
candidate was built (see
[What the candidate decides by itself](#what-the-candidate-decides-by-itself)).
Nobody raises it by hand. The value committed in `internal/release/freshness.go`
only reaches builds from source; it is the engine commit time of a promoted
release (`TZ=UTC git log -1 --date=iso-strict-local --format=%cd <sha>`) and may
be raised to that of a newer one, never beyond the live stable manifest's
`generated` (the candidate workflow refuses that). There is no maximum age:
releases are irregular and a floor that expired would stop offline mirrors and
old installs.

Mac continues to use ad-hoc signatures and the documented preview installation flow.
Production Developer ID/notarization requires a certificate and Apple credentials;
`sign-notarize-macos.sh` contains the runtime signing procedure. Windows uses
`CSC_LINK`/`CSC_KEY_PASSWORD` when configured. Configure and test these identities before
claiming signed/notarized distribution. Do not remove Spark's hypervisor
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
builds succeed on a manual run with `publish: true`. The publish job extends only the
index whose serial equals the repository variable `PKG_INDEX_MIN_SERIAL` and records
the new serial there afterwards, or stops the next publish until it is recorded
(see [Package recipes]({% link package-recipes.md %})). Existing package-index storage is retained; migrating package archives
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
