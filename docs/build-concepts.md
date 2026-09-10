---
layout: default
title: Build Concepts
nav_order: 3
---

# Build Concepts
{: .no_toc }

This page explains everything that happens when you run `jerboa build`, from
first principles. It assumes no prior knowledge of unikernels. If a build or a
boot fails, check [Troubleshooting]({% link troubleshooting.md %}) for the
exact error message.

## Table of contents
{: .no_toc .text-delta }

1. TOC
{:toc}

---

## What Is A Unikernel?

A container packages your app plus a userland and shares the host's Linux
kernel. A **unikernel** goes further in the other direction: it packages your
app plus a *minimal kernel* into a single bootable disk image, and runs it as
its own virtual machine.

Jerboa images boot **Nanos**, a small kernel purpose-built to run exactly one
application. There is:

- **no shell** — nothing interprets scripts inside the guest
- **no init system** — the kernel boots straight into your program
- **no other processes** — your program is the only thing that will ever run

What you gain: strong isolation (hardware VM boundary), small images, fast
boot, and a tiny attack surface. What you give up is described next, because it
is the single most important thing to understand about unikernels.

## The One-Process Model

Nanos is a **single-process kernel**. It implements threads, sockets, files,
and most Linux syscalls your program needs — but it has **no `fork` and no
`exec`**. A program that tries to launch a child process fails at runtime,
typically with:

```
popen failure: Cannot allocate memory
```

Practical consequences:

- **Setup tools that spawn helpers cannot run in the guest.** PostgreSQL's
  `initdb` fork/execs a bootstrap `postgres` process, so it can never run
  inside a unikernel. The solution is to do that work *at build time*: the
  `eyberg/postgresql` package ships a pre-initialized data directory baked
  into the image.
- **Docker-style entrypoint shell scripts do not work.** There is no `sh` to
  run them. The image must point directly at the real binary.
- **Process managers, cron daemons, and "worker pool via fork" designs do not
  work.** Multi-threading is fine; multi-process is not.

When you see a boot failure that mentions `popen`, `fork`, or
`unimplemented syscall`, this model is almost always the reason. `jerboa logs`
recognizes these messages and prints an explanation automatically.

## What A Build Produces

`jerboa build` assembles three things into a bootable **TFS disk image** (the
Nanos filesystem), using the `mkfs` tool from the kernel toolchain:

1. **Your program** — a Linux ELF binary (compiled from your source, taken
   from a package, or given directly as a file path).
2. **A filesystem tree** — your source files, package files (runtimes, shared
   libraries), and any empty directories you declared.
3. **A manifest** — the boot contract: which program to start, its
   `arguments`, its `environment`, and where every file lives. You do not
   write this manifest; Jerboa generates it.

The result is stored in the daemon's image store under `name:tag`, and
`jerboa run name:tag` boots it as a VM. Metadata like default memory, CPUs,
and port publishes ride along in the image manifest.

### What kind of binary can boot?

The program must be a **Linux ELF binary for x86_64** (or arm64). It can be:

- **Statically linked** (a Go binary with `CGO_ENABLED=0`, a Rust binary
  built for `x86_64-unknown-linux-musl`): nothing else is needed.
- **Dynamically linked**: then its *dynamic loader* (e.g.
  `/lib64/ld-linux-x86-64.so.2`) and *every shared library* it needs must
  also be inside the image. Packages ship these in their `sysroot/`.

A Windows `.exe` or a macOS binary can never boot. Jerboa's language drivers
always cross-compile for Linux, so you do not have to think about this unless
you bring your own binary.

## Language Drivers

`jerboa build <dir>` detects the project type from marker files, or you can
force it with `--lang` / `[build] lang`:

| Lang | Detected by | What it does |
|---|---|---|
| `go` | `go.mod` | `go build` with `CGO_ENABLED=0`, `GOOS=linux`, `-trimpath`, stripped symbols → static ELF |
| `rust` | `Cargo.toml` | `cargo build --release --target x86_64-unknown-linux-musl` → static ELF (install the target once: `rustup target add x86_64-unknown-linux-musl`) |
| `node` | `package.json` | Install production dependencies when manifests, lockfiles, runtime or platform change, or `node_modules/` is absent; ships your sources plus the Node runtime package (version from `engines.node`, default 20) |
| `python` | `pyproject.toml` or `requirements.txt` | `pip install` into `packages/` as **Linux x86_64 wheels** (whatever your host OS is); ships sources plus the Python runtime package (version from `requires-python`, default 3.12); sets `PYTHONPATH=/packages` |
| `raw` | never auto-detected | no compilation; the program comes from a package (see below) |

For interpreted languages the *program* is the runtime binary (node, python)
and your script is passed as the **entrypoint** — argv[1] of the runtime.

### Raw mode

`lang = "raw"` is for anything prebuilt: databases, JVM apps, compiled
binaries from the ops ecosystem. Nothing is compiled; the program is resolved
from the files of the packages you declare. Which binary runs is decided by,
in priority order:

1. `[program] path` in `unikernel.toml`, plus `[program] args`
2. the `Program`/`Args` that the ops package itself declares in its
   `package.manifest` — so for well-formed ops packages you can omit
   `[program]` entirely

## Packages

A **package** is a versioned bundle of prebuilt files — a language runtime, a
database server, shared libraries — that gets merged into your image. Two
sources exist:

- **`ops`** (default): the [nanovms/ops](https://ops.city) ecosystem at
  `repo.ops.city`. Thousands of prebuilt packages, addressed as
  `<namespace>/<name>:<version>` (e.g. `eyberg/postgresql:11.3.0`). Ops
  packages ship a `sysroot/` tree (libraries, data files) that is preserved
  in the image, and a `package.manifest` describing how to start the program.
- **`jerboa`**: the first-party index. It will become the default in the
  future; today `ops` is the default source.

Declare packages in `unikernel.toml` so builds need no flags:

```toml
[build]
lang = "raw"
pkgs = ["eyberg/postgresql:11.3.0"]
pkg_source = "ops"
```

or pass them per-build with `--pkg <ref>` (repeatable). Flags append to the
`pkgs` list; an explicit `--pkg-source` flag overrides `pkg_source`.

Useful commands:

```sh
jerboa pkg search postgres        # search the remote index
jerboa pkg get eyberg/mysql:5.7.29
jerboa pkg list                   # locally cached packages
```

Node and Python builds resolve their runtime package **automatically**
(`node:20`, `python:3.12`) — you only declare extra packages.

## The Program Path (And Its Trap)

For raw builds, `[program] path` is matched against the package's files by
exact path, path suffix, or basename. **Always give the full in-image path**
when the package ships an install tree:

```toml
[program]
path = "/usr/local/pgsql/bin/postgres"   # ✓
# path = "postgres"                      # ✗ may match a stub at the image root
```

Two things go wrong with bare names:

1. Some packages ship a same-named **launcher stub at the image root**
   (`/postgres`) alongside the real binary. The basename match finds the stub.
2. Programs like PostgreSQL locate their install prefix (`../share`, `../lib`)
   through their own executable path (`/proc/self/exe`), and resolve
   `$ORIGIN`-relative shared libraries the same way. Run from the wrong path,
   they fail with `could not locate my own executable path`.

Jerboa executes the program *from its real package location* when you give the
full path, so both mechanisms work.

## Environment Variables

Environment baked into the image comes from three layers; later layers win:

1. the ops package's `package.manifest` `Env` (e.g. `HOME`, paths the runtime
   expects)
2. the language driver (e.g. `PYTHONPATH=/packages` for pip installs)
3. `[env]` in `unikernel.toml` — your values, highest priority

`jerboa run -e KEY=VALUE` adds/overrides at run time.

## Disk Space And Writable Paths

By default the image is sized to its contents — **there is no free space**. If
the program writes at runtime (logs, temp files, a database), reserve room:

```toml
[build]
disk_size = "1G"    # minimum image size; the rest is free space
```

And every directory the program writes to must **exist in the image**:

```toml
[build]
dirs = ["/data", "/tmp/cache"]
```

`dirs` is also how you create **volume mount points**: a volume can only be
mounted onto a directory that already exists in the root image.

## Preflight Checks

Before assembling the image, `jerboa build` statically verifies things that
would otherwise only fail at boot, inside the guest, with a cryptic message:

- the program is a 64-bit Linux ELF for a supported architecture
- if dynamically linked: its interpreter is present in the image at the exact
  path the binary requests, and the full closure of shared libraries
  (`DT_NEEDED`, followed recursively) resolves against the image contents
- the entrypoint script (node/python) is among the packed files

Errors abort the build with an explanation and a fix hint. Skip with
`--no-preflight` if you know better than the check.

## Smoke Testing

`jerboa build . --name app --smoke` boots the image once right after building,
watches the serial output for a few seconds for known failure signatures
(fork/exec attempts, missing libraries, OOM), then stops and removes the test
VM. It turns "the build succeeded but does it boot?" into part of the build.

## Volumes And Seeding

Everything inside the root image is **ephemeral** — it lives and dies with the
VM. For data that must survive `jerboa rm`, mount a **volume** (a separate TFS
disk):

```sh
jerboa volume create data --size 1G
jerboa run app:latest -v data:/data
```

A fresh volume is empty. Mounting an empty volume over a path that has baked
data (e.g. a pre-initialized database at `/db`) *shadows* that data — so the
volume must be **seeded** once first. Create and seed in one step:

```sh
jerboa volume create pgdata --size 1G \
  --seed-pkg eyberg/postgresql:11.3.0 --src /db
jerboa run postgresql -v pgdata:/db --network pgnet -p 5432:5432
```

`--src /db` selects the in-package subtree whose *contents* become the volume
root; mounting the volume at `/db` restores them at the same place.

## Importing From Docker Images

`jerboa pkg from-docker` turns a binary inside a Docker image into a local
package (requires Docker on the build machine). Each file is stored at its real
absolute path inside the container, so the binary's interpreter (`/lib64/…`) and
shared libraries (`/lib/…`) land where the ELF references them.

```sh
# redis starts through a shell script (docker-entrypoint.sh), so point --file at
# the real binary — a unikernel runs exactly one program, with no shell.
jerboa pkg from-docker redis:7.2 redis:7.2 --file /usr/local/bin/redis-server
jerboa build . --name redis
```

The build reads a `unikernel.toml` that names the package and the program to run
(a from-docker package records no default program, so `[program]` is required):

```toml
[build]
lang = "raw"
pkgs = ["redis:7.2"]
pkg_source = "jerboa"

[program]
path = "/usr/local/bin/redis-server"
```

Without `--file`, the binary is derived from the image's own `Entrypoint`/`Cmd`
and resolved on the container's `PATH`. The shared-library closure is read
directly from the image's exported filesystem (no `ldd`/`cat` inside the image),
so it works even on `scratch` or distroless images that ship no shell or
coreutils. Images that start through a shell script (the common
`docker-entrypoint.sh` pattern) cannot be derived automatically — there is no
shell in a unikernel — so pass `--file` with the real binary the script
eventually launches.

## Scaffolding: `jerboa init`

```sh
jerboa init            # detects the language, writes a commented unikernel.toml
jerboa init --lang raw # template for package-driven builds
```

The generated file documents every field inline, including the pitfalls
described on this page.

## unikernel.toml Reference

```toml
[build]
lang = "go"              # go | node | python | rust | raw
entrypoint = "./cmd/api" # driver-specific entry (Go package path, node/python script)
args = []                # extra arguments for the build tool
run = ["npm run build"]  # shell commands before packaging (Dockerfile RUN analogue;
                         # these run on the BUILD machine, which has a shell — the guest does not)
pkgs = []                # packages to include (e.g. ["eyberg/postgresql:11.3.0"])
pkg_source = "ops"       # "ops" (default) or "jerboa"
disk_size = "1G"         # minimum image size (free space for runtime writes)
dirs = ["/data"]         # empty directories to create (mount points, scratch paths)

[program]                # raw builds only
path = "/usr/local/pgsql/bin/postgres"  # full in-image path of the program
args = ["-D", "/db"]     # argv[1..]; argv[0] is the program path
                         # omit the whole section to inherit Program/Args from
                         # the ops package's package.manifest

[env]                    # environment baked into the image (highest priority)
KEY = "value"

[run]                    # defaults inherited by `jerboa run` (flag > [run] > built-in)
memory = "512M"
cpus = 1
ports = ["5432:5432"]    # applied when the VM joins a --network and no -p is given

[[stages]]               # optional multi-stage builds
name = "frontend"
lang = "node"
# copy_from = [{ stage = "...", src = "...", dst = "..." }]
```

## Next

- [Troubleshooting]({% link troubleshooting.md %}) — common errors, decoded
- [Getting Started]({% link getting-started.md %})
- [CLI Reference]({% link cli-reference.md %})
