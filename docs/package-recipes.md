---
layout: default
title: Package recipes
description: "How first-party runtime packages are built, published and kept reproducible, and how to add a recipe."
nav_order: 12
---

# Package recipes

The first-party package index (`pkg_source = "jerboa"`) is built by the
**Package Build** workflow (`.github/workflows/packages.yml`) from recipes in
this repository. A recipe is two things:

- an entry in `scripts/release/packages.json`, which pins the source;
- a build script, `scripts/build-<package>.sh`, which turns that source into
  a program (and, optionally, the files it needs at runtime).

`scripts/release/archive-package.py` then runs `jerboa pkg create` on the
result, and `scripts/package-index.py` publishes it.

## The recipe entry

```json
{
  "package": "node",
  "version": "20.11.0",
  "source_url": "https://nodejs.org/dist/v20.11.0/node-v20.11.0-linux-x64.tar.xz",
  "sha256": "822780369d0ea309e7d218e41debbd1a03f8cdf354ebf8a4420e89f39cc2e612",
  "build_script": "scripts/build-node.sh",
  "arm64": {
    "source_url": "https://nodejs.org/dist/v20.11.0/node-v20.11.0-linux-arm64.tar.xz",
    "sha256": "f6df68c6793244071f69023a9b43a0cf0b13d65cbe86d55925c28e4134d9aafb"
  }
}
```

| Field | Meaning |
|---|---|
| `package`, `version` | Package reference. Letters, digits, `.`, `_` and `-` only. |
| `source_url`, `sha256` | The source the build starts from. The workflow downloads it and refuses to build if the SHA-256 differs. |
| `build_script` | The script that builds it. |
| `arches` | Optional. Architectures to build, default `["amd64", "arm64"]`. Use `["amd64"]` for a recipe whose upstream only ships an x86-64 binary. |
| `amd64`, `arm64` | Optional. Per-architecture overrides of `source_url` and `sha256`, for recipes that download a prebuilt binary rather than compile one. |

Each architecture builds on a native runner (`ubuntu-24.04` or
`ubuntu-24.04-arm`); nothing is cross-compiled.

## The build script contract

The workflow runs the script from the repository root with:

| Variable | Value |
|---|---|
| `PACKAGE_NAME`, `PACKAGE_VERSION` | From the recipe entry. |
| `SOURCE_URL` | A `file://` URL of the already verified source. Download it with `curl`, as for a remote URL. |
| `SOURCE_DATE_EPOCH` | A fixed timestamp (`1704067200`). Compilers use it for `__DATE__`/`__TIME__`, Python for `.pyc` files, and `pkg create` for archive entries. |

The script must leave:

- `dist/pkg/<name>/<version>/<binary>` — the program. `<binary>` is the
  package name unless `archive-package.py` maps it (`python` → `python3`,
  `redis` → `redis-server`, `sqlite` → `sqlite3`).
- `dist/pkg/<name>/<version>/rootfs/` — optional. Files the program needs at
  runtime, laid out at their final guest paths (for example
  `rootfs/usr/local/lib/python3.12/`). When it exists, the program is placed
  at `/usr/local/bin/<binary>`.

Build in a fixed directory (`/tmp/jerboa-pkg-<name>-<version>`), not one from
`mktemp -d`: compilers and build systems embed the build path in their
output (CPython records it in every extension module and in
`_sysconfigdata`), so a random directory makes every build different.

Shared libraries are not copied by the script. `pkg create` reads the ELF
headers (interpreter, `DT_NEEDED`, `RPATH`/`RUNPATH`) of the program and of
every ELF file under `rootfs/`, such as Python's extension modules, and adds
the loader and every library they need from the build environment at its
real path. A missing library fails the build.

## Reproducibility

The build runs inside `ubuntu:24.04` pinned by digest (the job's
`container:`), not on the runner, so the compiler and the libraries that end
up in the package change only when that digest is changed on purpose. With
the fixed `SOURCE_DATE_EPOCH`, rebuilding a recipe gives the same package.

`pkg create` writes archives that depend only on the files:

- entries sorted by path;
- owner and group 0, with no user or group names;
- modes 0755 for executables and 0644 for everything else;
- every timestamp set to `SOURCE_DATE_EPOCH` (or the Unix epoch when unset).

Every package also records a **content digest** in `meta.json` and in the
index (`content_digest`): SHA-256 over one `<mode> <sha256> <path>` line per
file, sorted by path. It names the files the package places in an image,
independent of how the archive is compressed. Images record it in their
package references next to the archive SHA-256, which remains the check for
the download itself. `jerboa build` rejects a package whose extracted files
no longer match it.

To check a recipe for reproducibility, build it twice and compare
`sha256` and `content_digest` in the two `meta.json` files.

## Files left out

A unikernel never compiles code or opens a window, so `pkg create` leaves out
these build-time files wherever they appear under a `lib/pythonX.Y/`
directory:

- `config-*` (static `libpython` and build headers);
- `test`, `idlelib`, `tkinter`, `turtledemo`, `ensurepip`, `lib2to3` and
  `pydoc_data`;
- optimized bytecode (`__pycache__/*.opt-1.pyc` and `*.opt-2.pyc`); the plain
  `.pyc` next to it is kept.

The same rules apply to packages from the ops repository. Recipes may delete
these files themselves (`build-python.sh` does) so the build log shows the
real package size.

## Publishing

Builds run on every push to `main` that touches a recipe, its entry,
`archive-package.py` or the workflow. Publishing is manual: run the workflow
with `publish` enabled and a package name (or `all`).

Published versions are immutable. `package-index.py` compares each built
package with the index entry for the same version and platform:

- **not published yet:** the archive is added and uploaded;
- **same archive or same content digest:** nothing changes;
- **different content:** the whole publish fails before anything is
  uploaded. Bump the version instead of rebuilding over it.

Archives are uploaded without `--clobber`; only `packages.json` and its
signature are replaced.

The publish job runs in the `release` environment, so it waits for a
reviewer, and signs `packages.json` with the release key into
`packages.json.minisig`. `jerboa` verifies that signature with the key it
embeds before it reads the index, and refuses an index without one. An
index served from this machine (`file://`, or http on loopback, as for a
local mirror) may be unsigned; it is then accepted with a warning.

## Example: adding memcached

1. Pin the source in `scripts/release/packages.json`:

   ```json
   {
     "package": "memcached",
     "version": "1.6.29",
     "source_url": "https://memcached.org/files/memcached-1.6.29.tar.gz",
     "sha256": "269643d518b7ba2033c7a1f66fdfc560d72725a2822194d90c8235408c443a49",
     "build_script": "scripts/build-memcached.sh"
   }
   ```

2. Add its build dependency (`libevent-dev`) to the `apt-get install` line of
   the **Install package build dependencies** step in
   `.github/workflows/packages.yml`.

3. Write `scripts/build-memcached.sh`:

   ```bash
   #!/usr/bin/env bash
   # Build memcached for unikernel images.
   set -euo pipefail

   NAME="${PACKAGE_NAME:-memcached}"
   VERSION="${PACKAGE_VERSION:-1.6.29}"
   SOURCE_URL="${SOURCE_URL:-https://memcached.org/files/memcached-${VERSION}.tar.gz}"

   PACKAGE_ROOT="$(pwd)"
   TMPDIR="/tmp/jerboa-pkg-${NAME}-${VERSION}"
   rm -rf "$TMPDIR" && mkdir -p "$TMPDIR"
   trap 'rm -rf "$TMPDIR"' EXIT

   curl -fSL -o "$TMPDIR/memcached.tar.gz" "$SOURCE_URL"
   tar -xf "$TMPDIR/memcached.tar.gz" -C "$TMPDIR"
   cd "$TMPDIR/memcached-${VERSION}"
   ./configure --prefix=/usr/local
   make -j"$(nproc)"

   OUTDIR="${PACKAGE_ROOT}/dist/pkg/${NAME}/${VERSION}"
   mkdir -p "$OUTDIR/rootfs/etc"
   cp memcached "$OUTDIR/memcached"
   chmod +x "$OUTDIR/memcached"
   # memcached refuses to run as root unless -u names a user it can look up.
   echo 'root:x:0:0:root:/:/sbin/nologin' > "$OUTDIR/rootfs/etc/passwd"
   ```

   `pkg create` adds `libevent` and the C library from the build container.
   Because the recipe has a `rootfs/`, the program lands at
   `/usr/local/bin/memcached`.

4. Build it locally in the same container before pushing (here for the
   machine's own architecture; `jerboa` must be a Linux build of the CLI for
   that architecture, for example
   `GOOS=linux CGO_ENABLED=0 go build -o dist/jerboa-linux ./cmd/jerboa`):

   ```sh
   docker run --rm -v "$PWD:/src:ro" -v "$PWD/dist/jerboa-linux:/jerboa:ro" \
     -e SOURCE_DATE_EPOCH=1704067200 \
     ubuntu@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac bash -c '
       apt-get update -q && apt-get install -y -q --no-install-recommends \
         ca-certificates curl python3 build-essential libevent-dev
       cp -a /src /work && cd /work && mkdir -p dist && cp /jerboa dist/jerboa
       PACKAGE_NAME=memcached PACKAGE_VERSION=1.6.29 bash scripts/build-memcached.sh
       PACKAGE_NAME=memcached PACKAGE_VERSION=1.6.29 python3 scripts/release/archive-package.py
       cat ~/.jerboa/packages/memcached/1.6.29/*/meta.json'
   ```

5. Use it from a project once it is published:

   ```toml
   [build]
   lang = "raw"
   pkg_source = "jerboa"
   pkgs = ["memcached:1.6.29"]

   [program]
   path = "/usr/local/bin/memcached"
   args = ["-u", "root", "-l", "0.0.0.0"]
   ```
