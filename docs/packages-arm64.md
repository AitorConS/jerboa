---
layout: default
title: Linux ARM64 packages on macOS
nav_order: 11
---

# Linux ARM64 packages on macOS

A package can be created from a local Linux ELF and sysroot without Docker.
Docker is needed only for `pkg from-docker` or to prepare a fixture. Builds and
Firecracker boots use the native macOS tools.

The guest program must be a Linux ARM64 ELF, not a native macOS Mach-O binary.
This workflow is available in the source-built macOS preview; it does not imply
that an older installed release includes these commands and metadata fields.

## Create a package

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o service ./cmd/service
jerboa pkg create service:1.2.3 ./service --platform linux/arm64 \
  --program-path /opt/service/bin/service

jerboa pkg create dynamic:2.3.4 ./sysroot/opt/service/bin/service \
  --sysroot ./sysroot --platform linux/arm64 \
  --program-path /opt/service/bin/service \
  --map ./config.json=/etc/service/config.json

jerboa pkg from-docker dynamic-docker:2.3.4 sha256:IMAGE_ID \
  --platform linux/arm64 --file /opt/service/bin/service
jerboa pkg list --source jerboa --platform linux/arm64 --output-json
```

`create` infers the platform from ELF64 and checks an explicit `--platform`.
`--libs` remains repeatable and places additional files at their basenames;
use `--map source=destination` for an exact guest path. The loader and transitive
libraries retain their guest paths, including paths reached through symlinks.
Resolution reads ELF headers, `PT_INTERP`, `DT_NEEDED`, `DT_RPATH`, `DT_RUNPATH`
and `$ORIGIN`; it never runs the program, `ldd`, or a Linux loader. Sysroot and
export symlinks resolve only inside that filesystem. Missing dependencies,
incompatible architectures, invalid executable formats and conflicting content
at the same destination fail the operation. Static inspection does not prove
support for every Linux syscall or symbol version in Nanos.

## Build and run an image

Use `[program]` to select the executable and arguments:

```toml
[build]
lang = "raw"
pkg_source = "jerboa"
pkgs = ["dynamic:2.3.4"]

[program]
path = "/opt/service/bin/service"
args = []
```

```sh
jerboa build . --name dynamic --platform linux/arm64
jerboa images inspect dynamic:latest
jerboa run dynamic:latest -p 127.0.0.1:8080:8080
```

Configure the isolated or development daemon with the signed macOS Firecracker
fork and its complete dependency package; see [Firecracker on macOS]({% link macos-firecracker.md %}).
Docker import never executes an entrypoint shell or automatically imports its
environment. A multiprocess Linux program does not become a Nanos application
by being packaged. PostgreSQL is outside this acceptance fixture.

## Platforms and image metadata

New packages store `platform`, `program_path` and `provenance`. Variants live in
`~/.jerboa/packages/<name>/<version>/linux-arm64/` or `linux-amd64/`.
Docker provenance contains the requested reference, immutable local image ID,
and a repository digest only when Docker reports one. A locally built image
without a repository digest has no invented digest. Host paths, credentials and
Docker environment variables are not package or image provenance.

Build resolution checks compatible local packages before making any index
request. Existing flat packages are checked against their ELF content in place;
they are not assigned ARM64 or rewritten. The remote/cache index is merged with
local metadata for discovery. `pkg get`, `search`, `list`, `remove`, `push` and
`load` accept `--platform` as well; use it to address an individual variant.

Image manifests and API responses expose `platform`, `architecture` and
`packages`, including concrete versions, archive SHA-256 and provenance. Only
references attached to files in the final build result are emitted. Legacy
manifests with no architecture retain their historical x86_64 interpretation;
absent provenance means unknown. Contradictory platform/architecture fields are
rejected. The daemon checks the received ELF and final guest tree independently
of CLI metadata, including with `--no-preflight`. Incompatible image/hypervisor
combinations fail before volume preparation or network reservation. x86_64 on
macOS requires the existing explicit QEMU emulation path.

## Load a package directly

For a self-contained package that needs no additional program arguments, build
and start it in one step. Select `jerboa` explicitly for locally created packages;
the default package source is `ops`.

```sh
jerboa pkg load service:1.2.3 --source jerboa --platform linux/arm64 -d
```

`-d` leaves the VM running and prints its ID after the build output. Use
`jerboa logs <vm-id>` to read output and `jerboa stop <vm-id>` to stop it.
For program arguments, configuration, or published ports, use the separate
build/run workflow above instead.

Like other daemon commands, `pkg load` resolves the endpoint from `--host`,
then `--socket`, `JERBOA_HOST`, the configuration file, or the platform default.
It uses the configured authentication token. For a separately running daemon:

```sh
jerboa --host unix:///tmp/jerboa-dev.sock pkg load service:1.2.3 \
  --source jerboa --platform linux/arm64 -d
```

## Push a local package

The destination must implement `POST /packages` with multipart archive and
metadata fields; this command is not a Docker registry push.

```sh
jerboa pkg push service:1.2.3 https://packages.example.com --platform linux/arm64
```

An explicit platform selects that variant. Without `--platform`, push prefers
the default platform, then a legacy package, then the sole downloaded variant;
ambiguous selection requires the flag. The default follows `JERBOA_PACKAGE_ARCH`
when set to `arm64` or `amd64`; otherwise it is ARM64 on native Apple Silicon
macOS and amd64 on other hosts. Selection is local and does not
download packages. Legacy content selected with an explicit platform is checked
against its ELF before upload. Versions and architecture variants are preserved.

## Reproducible real-guest acceptance

Fixtures are in `tests/fixtures/pkg-arm64`. Keep test binaries and tools separate
from installed binaries. Build the fixture image locally; no push is needed:

```sh
mkdir -p dist/astra-pkg-arm64
# Use the Go toolchain and signed Firecracker package from your checkout.
go build -o dist/astra-pkg-arm64/jerboa ./cmd/jerboa
go build -race -o dist/astra-pkg-arm64/jerboad ./cmd/jerboad
cp -R dist/macos-arm64/tools dist/astra-pkg-arm64/tools

docker build --platform linux/arm64 \
  --iidfile dist/astra-pkg-arm64/image-id tests/fixtures/pkg-arm64
export JERBOA_TEST_BIN="$PWD/dist/astra-pkg-arm64"
export JERBOA_FIRECRACKER_BIN="/path/to/signed/package/bin/firecracker"
export JERBOA_PKG_DOCKER_ID="$(cat dist/astra-pkg-arm64/image-id)"
python3 scripts/test-pkg-arm64.py
python3 scripts/test-firecracker-macos.py
python3 scripts/test-firecracker-network.py
```

The package acceptance script uses a fresh short `/tmp` path for `HOME`, daemon
socket and stores, retains evidence below `JERBOA_TEST_BIN`, and stops only its
own daemon and VMs. It imports a pinned Docker image, disables the Docker
endpoint, packages the same C executable from a local sysroot, builds all three
images through CLI/API, verifies metadata, requests HTTP, stops/restarts each
VM and repeats HTTP. It also tests coexistence with amd64 and rejects incompatible
builds and native runs. The C program calls its own dynamically linked library.
