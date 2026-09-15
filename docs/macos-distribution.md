---
layout: default
title: macOS Firecracker distribution
parent: macOS Apple Silicon
nav_order: 11
---

# macOS Firecracker distribution

The distribution is one relocatable ARM64 bundle containing Jerboa, its native
tools and the complete Firecracker/HVF package. It targets Apple Silicon and
macOS 26 or newer. Build it only after the release-candidate binaries from all
workstreams are final; the example input paths below are review artifacts, not
immutable release inputs.

## CI and the public R2 release channel

The main workflow builds native macOS releases when `VERSION.md` changes or
when a maintainer dispatches a publication. `build-macos-release` is a required
dependency of `publish-r2`: a failed build, VM regression, signature or
notarization check prevents the release channel from advancing.

Provision a dedicated runner with labels `self-hosted`, `macOS`, `ARM64` and
`hvf-macos26`. It needs macOS 26+, usable Hypervisor.framework, Apple Command
Line Tools, ARM64 ELF binutils on PATH, and Cargo/rustup. CI installs Go 1.27.1
and selects Rust 1.97.0. The fork is checked out at immutable commit
`6750fc374d4758a8181d75e5299e7137a110dd41`; update that pin together with the
integration tests when changing the fork API. Native dependencies are built
from the fork's SHA-256-locked sources and bundled with their licenses.

The runner's signing Keychain must contain Developer ID Application and
Developer ID Installer identities, and a stored notarytool profile. Configure
repository variables `MACOS_APPLICATION_IDENTITY`, `MACOS_INSTALLER_IDENTITY`
and `MACOS_NOTARY_PROFILE` to name them. Keep their private keys and Apple
credentials in the runner Keychain. These prerequisites are checked before
building; no ad-hoc package is published as a stable release.

CI builds the kernel and native tools, runs Go tests and installer/package
regressions, and exercises boot, shared networking and snapshots using the
relocated bundle. It then signs, notarizes, staples and verifies the installer
payload. R2 receives:

| Object | Purpose |
| --- | --- |
| `cli/vVERSION/jerboa-darwin-arm64` | Signed native CLI |
| `daemon/vVERSION/jerboad-darwin-arm64` | Signed native daemon |
| `macos/vVERSION/jerboa-VERSION-macos-arm64.pkg` | Complete native runtime and dependencies |
| `releases/vVERSION/manifest.json` and `.minisig` | Signed metadata for pinned installations |
| `channels/stable.json` and `.minisig` | Signed current release metadata |

The `macos.platforms.darwin-arm64` asset identifies the complete package.
Install that package for local VM execution: the standalone CLI and daemon
assets do not include Firecracker, its libraries or the kernel toolset.
All versioned artifacts are uploaded before the stable channel is updated.
The post-publication check downloads and hashes macOS artifacts alongside
Linux/Windows artifacts. A successful PR check alone does not publish to R2.

## Development bundle

The packaging command stages bytes but never builds, signs, installs, submits or
publishes them. It first verifies the Firecracker package's own checksums, writes
a complete bundle checksum inventory and creates a deterministic `tar.gz`:

```sh
epoch=$(git show -s --format=%ct HEAD)
python3 scripts/package-macos-distribution.py \
  --jerboa-dir dist/astra-pkg-arm64/review-fixes \
  --tools-dir dist/macos-arm64/tools \
  --firecracker-package ../firecracker-macos/experiments/hvf/build/astra-shared-network/review-fixes/package \
  --output-dir dist/macos-distribution/v0.0.0-dev \
  --version 0.0.0-dev \
  --source-date-epoch "$epoch"
python3 scripts/verify-macos-distribution.py \
  dist/macos-distribution/v0.0.0-dev/jerboa-0.0.0-dev-macos-arm64.tar.gz \
  --mode adhoc
```

The layout puts `jerboad`, Firecracker and `tools/` together under `bin/`, which
matches Jerboa's relative lookup. Firecracker's `@loader_path/../lib` references
resolve to the bundled libraries. Its source archives, lockfile, manifest,
licenses and upstream checksums remain under `share/firecracker/`.

Given identical input bytes, version, Git commit, script and
`SOURCE_DATE_EPOCH`, staging and archive bytes are deterministic. This claim is
limited to packaging: the example reuses prebuilt Jerboa/tools/Firecracker
inputs. To claim that the compiled bytes are reproducible, use the source build
below and keep its comparison output with the release report.

## Reproducible source build

`scripts/build-macos-reproducible.py` compiles every ARM64 component from
source instead of staging prebuilt bytes: `jerboa`/`jerboad` (Go), the native
tools and kernel/boot images (kernel Makefiles), glib/libintl (Meson), and
Firecracker/HVF (Cargo). It never signs with an identity, notarizes, installs or
publishes.

1. Freeze the sources once, before other work changes them. The snapshot
   contains tracked, modified and untracked non-ignored files of `jerboa` and
   `firecracker-macos`, the vendored kernel sources, the SHA-256-locked native
   source archives and the few ignored generated headers the build needs. Dirty
   changes are included on purpose; `content-manifest.txt` and `tree-digest.txt`
   identify the exact content, not just `HEAD`:

   ```sh
   python3 scripts/build-macos-reproducible.py snapshot --output /path/to/snapshot
   ```

   Extraction validates the complete symlink graph before writing, rejects
   duplicate paths and files below symlink parents, and requires an empty
   destination. Relative links confined to the extracted tree remain supported.

2. Build twice, in two new directories outside any Git checkout. Each build
   extracts the snapshot, refuses an existing root and uses fresh Go, Cargo and
   Meson caches and target directories. Only authenticated dependency archives
   are read from shared locations: Go module zips (checked against `go.sum`),
   crate archives and the Git database pinned by `Cargo.lock`, and the native
   archives pinned by `sources.json`. No network is used:

   ```sh
   python3 scripts/build-macos-reproducible.py build --snapshot /path/to/snapshot \
     --work /private/tmp/jerboa-repro/a --version 0.0.0-dev
   python3 scripts/build-macos-reproducible.py build --snapshot /path/to/snapshot \
     --work /private/tmp/jerboa-repro/second-b --version 0.0.0-dev
   python3 scripts/build-macos-reproducible.py compare \
     /private/tmp/jerboa-repro/a /private/tmp/jerboa-repro/second-b --output comparison.json
   ```

`scripts/macos-reproducible-lock.json` pins the environment: Go 1.26.0, Rust
1.97.0, Apple clang/ld/strip/codesign from Command Line Tools, macOS SDK 26.5,
`aarch64-elf` binutils 2.47, make/xxd/awk, the macOS build and SHA-256 hashes of
those executables. `build` refuses a mismatch unless you pass
`--allow-toolchain-drift`; `compare` never reports such a build as reproducible.
Regenerate the lock only as a reviewed maintainer change (`lock --output`).

The recipe removes build-root and time dependence in these ways:

- C: `-ffile-prefix-map=<root>=/build`; `ZERO_AR_DATE=1`; `SOURCE_DATE_EPOCH`
  is the frozen Jerboa commit time; `TZ=UTC`, `LC_ALL=C` and a private `HOME`.
- Mach-O `LC_UUID`: Apple ld hashes the output including the debug map's object
  paths, and a later `strip` does not recompute it. Host tools and Firecracker
  are therefore linked with `-Wl,-S`, then stripped with `strip -S`.
- Go: `-trimpath -buildvcs=false`, `GOTOOLCHAIN=local`, `GOENV=off` and no
  root-specific `CGO_*` flags, because their values feed the Go build ID and
  `LC_UUID`.
- Rust: `--remap-path-prefix` for the build root, the private `CARGO_HOME` and
  the source tree, one codegen unit and `--locked --offline`.
- glib: installed with `DESTDIR` under the logical prefix
  `/opt/jerboa-hvf-native`. Only that constant appears in the libraries; the
  packager rewrites install names to `@loader_path`.
- Kernel: the snapshot has no `.git`, so `gitversion` reports the frozen `HEAD`
  plus `+snapshot.<tree digest>`, and inert `.git/index`/`.git/HEAD`
  placeholders satisfy `kernel.mk`'s prerequisites.

`compare` checks every file of the Jerboa, tools and Firecracker outputs and of
the final bundle twice: the ad-hoc signed bytes (`sha256`) and, for Mach-O files,
the bytes with the signature removed (`unsigned_sha256`). It also compares the
archive hash. Pre-signature equality is what a later Developer ID signature
would be applied to. Signed equality is only true of ad-hoc signatures made
without a timestamp. A Developer ID signature with a secure timestamp will
differ between runs by design.

`bin/tools/x86` is not rebuilt: no x86_64 guest cross toolchain is pinned. Its
three files are copied from the signed release artifacts fetched by
`scripts/fetch-x86-tools.go`, and the lock verifies their hashes.

The claim is bit-for-bit reproducibility on this pinned macOS ARM64 environment
only. Other Macs, other Command Line Tools or SDK versions, and full Xcode are
unverified until the comparison is repeated there.

## Verification

The ad-hoc verifier checks every checksum, Mach-O ARM64 architecture, strict code
signature validity, exact Firecracker Hypervisor entitlement, absence of
unexpected Jerboa entitlements, and all dynamic-library paths. It extracts to a
new temporary directory and runs only `--version` for Jerboa, the daemon and
Firecracker. It does not start or replace the managed daemon, boot a VM, install
under `/usr/local`, or contact Apple.

Run the functional smoke against the relocated bundle as a separate acceptance
step. The test creates its own short-lived HOME, daemon socket, image store,
ports and VMs, then builds an ARM64 fixture and proves HTTP from the guest:

```sh
export PATH="$PWD/../firecracker-macos/experiments/hvf/build/guest-tools/go/bin:$PATH"
bundle="$PWD/dist/macos-distribution/v0.0.0-dev/jerboa-0.0.0-dev-macos-arm64"
JERBOA_TEST_BIN="$bundle/bin" \
JERBOA_FIRECRACKER_BIN="$bundle/bin/firecracker" \
python3 scripts/test-firecracker-macos.py
```

## Developer ID and notarized release

Ad-hoc signing is suitable for local development evidence, not public release.
A distributable release needs a trusted `Developer ID Application` identity for
every Mach-O, a `Developer ID Installer` identity for the installer, Hardened
Runtime, the Firecracker Hypervisor entitlement, successful Apple notarization,
and a stapled ticket. Use a copy of the reviewed bundle:

```sh
scripts/sign-notarize-macos.sh \
  --input dist/macos-distribution/v1.2.3/jerboa-1.2.3-macos-arm64 \
  --output dist/macos-distribution/v1.2.3/jerboa-1.2.3-macos-arm64-signed \
  --application-identity 'Developer ID Application: Example Corp (TEAMID)' \
  --pkg dist/macos-distribution/v1.2.3/jerboa-1.2.3-macos-arm64.pkg \
  --installer-identity 'Developer ID Installer: Example Corp (TEAMID)'
```

That command signs locally and creates a signed installer, but does not contact
Apple. Only an explicitly authorized run may add:

```sh
  --notarize --notary-profile jerboa-notary
```

This invokes `notarytool submit --wait`, staples and validates the ticket, then
asks Gatekeeper to assess the installer. Never put Apple credentials in command
arguments or the repository; create the named Keychain profile separately.
The signing helper regenerates the bundle checksum inventory after changing the
Mach-O bytes. It renames the embedded Firecracker `manifest.json` and
`SHA256SUMS` with an `upstream-adhoc` suffix and adds `SIGNING-NOTICE.json`, so
metadata for pre-signing bytes cannot be mistaken for the signed inventory.
After notarization, verify both the signed bundle and the stapled
installer (this deliberately fails for an unstapled or ad-hoc artifact):

```sh
python3 scripts/verify-macos-distribution.py \
  dist/macos-distribution/v1.2.3/jerboa-1.2.3-macos-arm64-signed \
  --mode release \
  --installer-pkg dist/macos-distribution/v1.2.3/jerboa-1.2.3-macos-arm64.pkg
```

Release verification expands the installer without installing it and compares
every payload path, byte hash and file mode with the already verified signed
bundle. Installer scripts and payload files outside
`/usr/local/libexec/jerboa` are rejected, so an unrelated validly notarized
package cannot satisfy release verification.

Also run the isolated Firecracker functional suites documented in
[Firecracker on macOS]({% link macos-firecracker.md %}).

Before publishing, review the embedded Firecracker README and manifest. A
notarized wrapper cannot turn an experimental dependency with deferred network
or stability acceptance into a production-supported release. Preserve all
license/source material and publish the final checksum file through an
authenticated release channel.
