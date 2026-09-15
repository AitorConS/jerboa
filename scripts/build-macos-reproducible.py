#!/usr/bin/env python3
"""Reproducible source build of the Jerboa + Firecracker/HVF macOS ARM64 bundle.

Subcommands:

  snapshot  Freeze Jerboa and firecracker-macos working trees (tracked, modified
            and untracked non-ignored files plus pinned vendored sources) into a
            tar with a content manifest. Dirty changes are included on purpose.
  lock      Fingerprint the local toolchain into a lock file (maintainer step).
  build     Compile everything from a frozen snapshot into a new build root
            using only the locked toolchain, fresh caches and no network.
  compare   Compare two build records: signed bytes and pre-signature bytes.

It never signs with an identity, notarizes, installs or publishes. Ad-hoc
signatures are applied exactly as in the development bundle. The reproducibility
claim is limited to the environment pinned by the lock file.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import platform
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile

SCRIPT = pathlib.Path(__file__).resolve()
JERBOA = SCRIPT.parents[1]
PACK = JERBOA.parent
FIRECRACKER = PACK / "firecracker-macos"
DEFAULT_LOCK = SCRIPT.with_name("macos-reproducible-lock.json")

# Logical install prefix for glib. Only install names/strings see it; the
# packager rewrites install names to @loader_path, so no build path leaks.
NATIVE_PREFIX = "/opt/jerboa-hvf-native"
MACHO_SUFFIXES = (".dylib",)
CLT = pathlib.Path("/Library/Developer/CommandLineTools")
SYSTEM_PATH = "/usr/bin:/bin:/usr/sbin:/sbin"


# --------------------------------------------------------------------------- utils

def sha256_file(path: pathlib.Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def output(*argv, env=None, cwd=None) -> str:
    return subprocess.check_output([str(a) for a in argv], text=True, env=env, cwd=cwd,
                                   stderr=subprocess.STDOUT).strip()


class Log:
    def __init__(self, path: pathlib.Path):
        self.handle = path.open("a")

    def run(self, argv, env, cwd=None):
        argv = [str(a) for a in argv]
        self.handle.write(f"$ (cwd={cwd}) {' '.join(argv)}\n")
        self.handle.flush()
        result = subprocess.run(argv, env=env, cwd=cwd, stdout=self.handle, stderr=subprocess.STDOUT)
        if result.returncode:
            raise SystemExit(f"command failed ({result.returncode}): {' '.join(argv)}; see {self.handle.name}")


def is_macho(path: pathlib.Path) -> bool:
    with path.open("rb") as source:
        magic = source.read(4)
    return magic in (b"\xcf\xfa\xed\xfe", b"\xfe\xed\xfa\xcf", b"\xca\xfe\xba\xbe", b"\xbe\xba\xfe\xca")


def unsigned_sha256(path: pathlib.Path) -> str:
    """Hash of the Mach-O bytes with the code signature removed (pre-signature bytes)."""
    with tempfile.TemporaryDirectory(prefix="jerboa-unsigned-") as temporary:
        copy = pathlib.Path(temporary) / path.name
        shutil.copyfile(path, copy)
        subprocess.run(["/usr/bin/codesign", "--remove-signature", str(copy)], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        return sha256_file(copy)


def inventory(root: pathlib.Path) -> dict:
    """Per-file signed/pre-signature hashes of a tree (regular files only)."""
    result = {}
    for path in sorted(p for p in root.rglob("*") if p.is_file() and not p.is_symlink()):
        relative = path.relative_to(root).as_posix()
        entry = {"sha256": sha256_file(path), "mode": oct(stat.S_IMODE(path.stat().st_mode))}
        if is_macho(path):
            entry["unsigned_sha256"] = unsigned_sha256(path)
        result[relative] = entry
    return result


# ------------------------------------------------------------------------ snapshot

def git_lines(repo: pathlib.Path, *argv: str) -> list[str]:
    return output("git", "-C", repo, *argv).splitlines()


# Git-ignored files that the build nevertheless needs (generated upstream headers).
IGNORED_REQUIRED_FILES = (
    "firecracker-macos/src/hvf-vmm/vendor/libslirp/src/libslirp-version.h",
)


def snapshot_file_list(pack: pathlib.Path) -> list[str]:
    names = set()
    for repository in ("jerboa", "firecracker-macos"):
        for name in git_lines(pack / repository, "ls-files", "-co", "--exclude-standard"):
            names.add(f"{repository}/{name}")
    # Ignored but required, pinned inputs: kernel vendored sources (without .git)
    # and the SHA-256-locked native source archives.
    for extra in ("jerboa/kernel/vendor", "firecracker-macos/experiments/hvf/build/distribution/downloads"):
        for current, directories, files in os.walk(pack / extra):
            directories[:] = sorted(d for d in directories if d != ".git")
            for name in files:
                names.add((pathlib.Path(current) / name).relative_to(pack).as_posix())
    names.update(IGNORED_REQUIRED_FILES)
    return sorted(n for n in names if os.path.islink(pack / n) or (pack / n).is_file())


def manifest_lines(archive: pathlib.Path) -> list[str]:
    lines = []
    with tarfile.open(archive) as source:
        for member in sorted(source.getmembers(), key=lambda m: m.name):
            if member.isfile():
                extracted = source.extractfile(member)
                assert extracted is not None
                digest, kind = sha256_bytes(extracted.read()), "f"
            elif member.issym():
                digest, kind = sha256_bytes(member.linkname.encode()), "l"
            else:
                continue
            mode = 0o755 if member.mode & 0o111 else 0o644
            lines.append(f"{digest} {kind} {mode:o} {member.name}")
    return lines


def tree_digest(lines: list[str]) -> str:
    return sha256_bytes("".join(line + "\n" for line in lines).encode())


def cmd_snapshot(args) -> None:
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    names = snapshot_file_list(args.pack.resolve())
    (out / "filelist.txt").write_text("\n".join(names) + "\n")
    archive = out / "source-snapshot.tar"
    subprocess.run(["tar", "-c", "--no-xattrs", "--no-mac-metadata", "-f", str(archive), "-T", str(out / "filelist.txt")],
                   cwd=args.pack, check=True, env=dict(os.environ, COPYFILE_DISABLE="1"))
    for repository, label in (("jerboa", "jerboa"), ("firecracker-macos", "firecracker")):
        (out / f"{label}-HEAD.txt").write_text(output("git", "-C", args.pack / repository, "rev-parse", "HEAD") + "\n")
        (out / f"{label}-status.txt").write_text(
            output("git", "-C", args.pack / repository, "status", "--porcelain=v1", "-uall") + "\n")
    (out / "source-date-epoch.txt").write_text(
        output("git", "-C", args.pack / "jerboa", "show", "-s", "--format=%ct", "HEAD") + "\n")
    finish_snapshot(out)


def finish_snapshot(out: pathlib.Path) -> None:
    lines = manifest_lines(out / "source-snapshot.tar")
    (out / "content-manifest.txt").write_text("".join(line + "\n" for line in lines))
    (out / "tree-digest.txt").write_text(tree_digest(lines) + "\n")
    print(json.dumps({"snapshot": str(out), "files": len(lines), "tree_digest": tree_digest(lines)}))


def safe_extract_snapshot(archive: pathlib.Path, target: pathlib.Path) -> None:
    if target.is_symlink() or (target.exists() and (not target.is_dir() or any(target.iterdir()))):
        raise SystemExit("snapshot extraction requires an empty, non-symlink directory")
    with tarfile.open(archive) as source:
        members = source.getmembers()
        entries = {}
        for member in members:
            path = pathlib.PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts or not path.parts:
                raise SystemExit(f"unsafe snapshot member: {member.name}")
            if path.parts[0] not in ("jerboa", "firecracker-macos"):
                raise SystemExit(f"unexpected snapshot root: {member.name}")
            if not (member.isfile() or member.isdir() or member.issym()):
                raise SystemExit(f"unsupported snapshot member type: {member.name}")
            if str(path) in entries:
                raise SystemExit(f"duplicate snapshot member: {member.name}")
            entries[str(path)] = member
            if member.issym():
                resolved = os.path.normpath(str(path.parent / member.linkname))
                if member.linkname.startswith("/") or resolved == ".." or resolved.startswith("../"):
                    raise SystemExit(f"unsafe snapshot link: {member.name} -> {member.linkname}")
        # Validate the complete link graph before writing anything. Lexical
        # normalization alone misses `a -> ..`, `b -> a/..` escapes.
        def resolve_link(parts, stack=()):
            resolved = []
            for part in parts:
                if part == "..":
                    if not resolved:
                        raise SystemExit("unsafe snapshot link: escapes extraction root")
                    resolved.pop()
                elif part != ".":
                    resolved.append(part)
                    name = "/".join(resolved)
                    entry = entries.get(name)
                    if entry is not None and entry.issym():
                        if name in stack or len(stack) >= 40:
                            raise SystemExit("unsafe snapshot link: cycle or excessive depth")
                        resolved = resolve_link(resolved[:-1] + list(pathlib.PurePosixPath(entry.linkname).parts), stack + (name,))
            return resolved

        for name, member in entries.items():
            path = pathlib.PurePosixPath(name)
            for parent in path.parents:
                entry = entries.get(str(parent))
                if entry is not None and not entry.isdir():
                    raise SystemExit(f"unsafe snapshot member parent: {name}")
            if member.issym():
                resolve_link(list(path.parts))
        for member in members:
            destination = target.joinpath(*pathlib.PurePosixPath(member.name).parts)
            if member.isdir():
                destination.mkdir(parents=True, exist_ok=True)
            elif member.issym():
                destination.parent.mkdir(parents=True, exist_ok=True)
                destination.symlink_to(member.linkname)
            else:
                destination.parent.mkdir(parents=True, exist_ok=True)
                extracted = source.extractfile(member)
                assert extracted is not None
                with extracted, destination.open("xb") as handle:
                    shutil.copyfileobj(extracted, handle)
                destination.chmod(0o755 if member.mode & 0o111 else 0o644)


def verify_snapshot(snapshot: pathlib.Path) -> str:
    expected = (snapshot / "tree-digest.txt").read_text().strip()
    recorded = (snapshot / "content-manifest.txt").read_text().splitlines()
    actual = manifest_lines(snapshot / "source-snapshot.tar")
    if recorded != actual or tree_digest(actual) != expected:
        raise SystemExit("snapshot archive does not match its content manifest/tree digest")
    return expected


# --------------------------------------------------------------------------- lock

def lock_paths(lock: dict) -> dict:
    def resolve(value: str) -> pathlib.Path:
        return pathlib.Path(os.path.expanduser(value)) if value.startswith(("/", "~")) else (JERBOA / value).resolve()
    return {key: resolve(value) for key, value in lock["paths"].items()}


DEFAULT_PATHS = {
    "go_root": "../firecracker-macos/experiments/hvf/build/guest-tools/go",
    "rustup_home": "../firecracker-macos/experiments/hvf/build/rustup",
    "cargo_dependency_home": "../firecracker-macos/experiments/hvf/build/cargo",
    "go_module_download_cache": "~/go/pkg/mod/cache/download",
    "aarch64_elf_binutils": "/opt/homebrew/opt/aarch64-elf-binutils/bin",
    "x86_tools": "dist/claude-closure/reproducibilidad/pinned-inputs/tools-x86",
}


def fingerprint(paths: dict) -> dict:
    rust = paths["rustup_home"] / "toolchains/1.97.0-aarch64-apple-darwin"
    go = paths["go_root"]
    files = {
        "go/bin/go": go / "bin/go",
        **{f"go/pkg/tool/darwin_arm64/{t}": go / "pkg/tool/darwin_arm64" / t for t in ("compile", "link", "asm", "cgo")},
        "rust/bin/rustc": rust / "bin/rustc",
        "rust/bin/cargo": rust / "bin/cargo",
        **{f"rust/lib/{p.name}": p for p in sorted((rust / "lib").glob("librustc_driver-*.dylib"))},
        **{f"clt/{t}": CLT / "usr/bin" / t for t in ("clang", "ld", "strip", "install_name_tool", "codesign_allocate", "libtool", "ar", "otool")},
        "clt/SDKSettings.json": CLT / "SDKs/MacOSX.sdk/SDKSettings.json",
        "system/codesign": pathlib.Path("/usr/bin/codesign"),
        "system/make": pathlib.Path("/usr/bin/make"),
        "system/xxd": pathlib.Path("/usr/bin/xxd"),
        "system/awk": pathlib.Path("/usr/bin/awk"),
        "system/tar": pathlib.Path("/usr/bin/tar"),
        **{f"binutils/aarch64-elf-{t}": paths["aarch64_elf_binutils"] / f"aarch64-elf-{t}" for t in ("as", "ld", "objcopy", "objdump", "strip")},
    }
    rustc = rust / "bin/rustc"
    return {
        "host": {"product_version": output("sw_vers", "-productVersion"), "build_version": output("sw_vers", "-buildVersion"),
                 "machine": platform.machine()},
        "versions": {
            "go": output(go / "bin/go", "version", env={"PATH": SYSTEM_PATH, "GOTOOLCHAIN": "local", "HOME": "/nonexistent"}),
            "rustc": output(rustc, "-vV"),
            "clang": output("/usr/bin/clang", "--version"),
            "sdk_version": output("/usr/bin/xcrun", "--show-sdk-version"),
            "sdk_build": output("/usr/bin/xcrun", "--show-sdk-build-version"),
            "python3": output("/usr/bin/python3", "--version"),
        },
        "files": {name: sha256_file(path) for name, path in files.items()},
        "x86_tools": {p.name: sha256_file(p) for p in sorted(paths["x86_tools"].iterdir()) if p.is_file()},
    }


def cmd_lock(args) -> None:
    if args.output.exists():
        raise SystemExit(f"refusing to overwrite lock: {args.output}")
    lock = {
        "format_version": 1,
        "scope": ("Bit-for-bit reproducibility is claimed only on macOS ARM64 with exactly these toolchain "
                  "bytes, SDK and host build; other environments are unverified."),
        "paths": DEFAULT_PATHS,
        "macosx_deployment_target": "26.0",
        "rust_toolchain": "1.97.0",
        "native_prefix": NATIVE_PREFIX,
        "x86_tools_provenance": ("prebuilt signed Jerboa release kernel/boot images fetched by scripts/fetch-x86-tools.go; "
                                 "not rebuilt here (no x86_64 guest cross toolchain is pinned); preserved by hash"),
    }
    lock["fingerprint"] = fingerprint(lock_paths(lock))
    args.output.write_text(json.dumps(lock, indent=2, sort_keys=True) + "\n")
    print(args.output)


def check_lock(lock: dict) -> list[str]:
    actual = fingerprint(lock_paths(lock))
    expected = lock["fingerprint"]
    drift = []
    for section in ("host", "versions", "files", "x86_tools"):
        for key in sorted(set(expected[section]) | set(actual[section])):
            if expected[section].get(key) != actual[section].get(key):
                drift.append(f"{section}.{key}: locked={expected[section].get(key)!r} actual={actual[section].get(key)!r}")
    return drift


# -------------------------------------------------------------------------- build

def base_env(root: pathlib.Path, lock: dict, epoch: int, toolbin: pathlib.Path) -> dict:
    paths = lock_paths(lock)
    for name in ("home", "tmp"):
        (root / name).mkdir(exist_ok=True)
    return {
        "PATH": f"{toolbin}:{paths['go_root']}/bin:{SYSTEM_PATH}",
        "HOME": str(root / "home"),
        "TMPDIR": str(root / "tmp"),
        "LANG": "C", "LC_ALL": "C", "TZ": "UTC",
        "SOURCE_DATE_EPOCH": str(epoch),
        "ZERO_AR_DATE": "1",
        "MACOSX_DEPLOYMENT_TARGET": lock["macosx_deployment_target"],
        "DEVELOPER_DIR": str(CLT),
        # Never let git discover a repository above the build root.
        "GIT_CEILING_DIRECTORIES": str(root.parent),
    }


def prefix_map(root: pathlib.Path) -> str:
    return f"-ffile-prefix-map={root}=/build"


def build_kernel(root: pathlib.Path, env: dict, log: Log, snapshot_head: str, digest: str) -> dict:
    jerboa = root / "src/jerboa"
    fake_git = root / "toolbin/git"
    # kernel.mk embeds `git rev-parse HEAD`; the snapshot has no .git. Report the
    # frozen HEAD plus the snapshot tree digest instead, deterministically.
    fake_git.write_text(f"#!/bin/sh\necho '{snapshot_head}+snapshot.{digest[:12]}'\n")
    fake_git.chmod(0o755)
    # kernel.mk lists .git/index and .git/HEAD as prerequisites and GNU make does
    # not forward -o to sub-makes, so provide inert placeholders. GIT is
    # overridden and GIT_CEILING_DIRECTORIES prevents any real repository use.
    placeholder = jerboa / ".git"
    placeholder.mkdir()
    for name in ("index", "HEAD"):
        (placeholder / name).write_text("jerboa reproducible build placeholder; not a repository\n")
        os.utime(placeholder / name, (0, 0))
    kenv = dict(env, CFLAGS=prefix_map(root))
    make = ["/usr/bin/make", "-j8", f"GIT={fake_git}"]
    # Host tools first, linked without a debug map: Apple ld derives LC_UUID from
    # the output including N_OSO object paths, which contain the build root, and
    # a later strip does not recompute it. LDFLAGS must not reach the ELF kernel link.
    log.run([*make, "-C", jerboa / "kernel", "PLATFORM=virt", "tools"], dict(kenv, LDFLAGS="-Wl,-S"))
    log.run([*make, "-C", jerboa / "kernel", "PLATFORM=virt", "kernel"], kenv)
    boot = jerboa / "kernel/output/platform/virt/boot-stub.img"
    log.run([*make, "-C", jerboa / "kernel/platform/virt", "PLATFORM=virt", boot], kenv)
    tools = root / "out/tools"
    tools.mkdir(parents=True)
    for name in ("mkfs", "dump"):
        shutil.copyfile(jerboa / "kernel/output/tools/bin" / name, tools / name)
        (tools / name).chmod(0o755)
        # Drop the linker debug map (absolute object paths) like the FC packager.
        log.run(["/usr/bin/strip", "-S", tools / name], env)
    shutil.copyfile(jerboa / "kernel/output/platform/virt/bin/kernel.img", tools / "kernel.img")
    shutil.copyfile(boot, tools / "boot.img")
    (tools / "platform.txt").write_text("darwin-arm64\n")
    shutil.copyfile(jerboa / "kernel/VERSION", tools / "kernel-version.txt")
    return {"tools_dir": tools}


def build_go(root: pathlib.Path, env: dict, log: Log, lock: dict, version: str) -> pathlib.Path:
    paths = lock_paths(lock)
    out = root / "out/jerboa"
    out.mkdir(parents=True)
    genv = dict(env,
                GOROOT=str(paths["go_root"]), GOTOOLCHAIN="local", GOENV="off", GOTELEMETRY="off",
                GOCACHE=str(root / "gocache"), GOMODCACHE=str(root / "gomodcache"), GOPATH=str(root / "gopath"),
                # Modules come from the local download cache; go.sum authenticates every zip.
                GOPROXY=f"file://{paths['go_module_download_cache']}", GOSUMDB="off", GONOSUMDB="", GOPRIVATE="",
                GOFLAGS="-mod=readonly -trimpath -buildvcs=false -modcacherw",
                # No build-root-specific CGO_* flags: their values feed the Go action ID,
                # hence the build ID and LC_UUID. -trimpath already rewrites cgo paths.
                CGO_ENABLED="1", GOOS="darwin", GOARCH="arm64", CC="/usr/bin/clang")
    for name in ("jerboa", "jerboad"):
        log.run(["go", "build", "-ldflags", f"-s -w -X main.version={version}", "-o", out / name, f"./cmd/{name}"],
                genv, cwd=root / "src/jerboa")
    return out


def build_glib(root: pathlib.Path, env: dict, log: Log) -> pathlib.Path:
    fc = root / "src/firecracker-macos"
    locked = json.loads((fc / "experiments/hvf/distribution/sources.json").read_text())
    downloads = fc / "experiments/hvf/build/distribution/downloads"
    for name, source in sorted(locked.items()):
        if sha256_file(downloads / name) != source["sha256"]:
            raise SystemExit(f"native source checksum mismatch: {name}")
    work = root / "native"
    work.mkdir()
    with tarfile.open(downloads / "glib-2.88.3.tar.xz") as archive:
        for member in archive.getmembers():
            if member.name.startswith("/") or ".." in pathlib.PurePosixPath(member.name).parts:
                raise SystemExit(f"unsafe glib member {member.name}")
        archive.extractall(work)
    source = work / "glib-2.88.3"
    wraps = source / "subprojects/packagecache"
    wraps.mkdir(exist_ok=True)
    for name in locked:
        if not name.endswith(".whl") and not name.startswith("glib-"):
            shutil.copyfile(downloads / name, wraps / name)
    venv = work / "tools"
    log.run(["/usr/bin/python3", "-m", "venv", venv], env)
    log.run([venv / "bin/python3", "-m", "pip", "install", "--no-index", "--no-deps", "--disable-pip-version-check",
             *[downloads / name for name in sorted(locked) if name.endswith(".whl")]], env)
    nenv = dict(env, PATH=f"{venv}/bin:{SYSTEM_PATH}", CC="/usr/bin/clang", CXX="/usr/bin/clang++",
                PKG_CONFIG_LIBDIR=str(work / "empty-pkgconfig"), CFLAGS=prefix_map(root), CXXFLAGS=prefix_map(root))
    build = work / "build"
    destdir = work / "destdir"
    log.run(["meson", "setup", build, source, f"--prefix={NATIVE_PREFIX}", "--libdir=lib",
             "--buildtype=release", "--wrap-mode=nodownload",
             "--force-fallback-for=libpcre2-8,libffi,intl", "-Dtests=false",
             "-Dinstalled_tests=false", "-Ddocumentation=false", "-Dman-pages=disabled",
             "-Dintrospection=disabled", "-Dnls=disabled", "-Dsysprof=disabled",
             "-Ddtrace=disabled", "-Dsystemtap=disabled", "-Dselinux=disabled",
             "-Dlibmount=disabled"], nenv)
    log.run(["meson", "compile", "-C", build, "-j", "8"], nenv)
    log.run(["meson", "install", "-C", build, "--no-rebuild", "--destdir", destdir], nenv)
    return destdir


def build_firecracker(root: pathlib.Path, env: dict, log: Log, lock: dict, destdir: pathlib.Path) -> pathlib.Path:
    paths = lock_paths(lock)
    fc = root / "src/firecracker-macos"
    cargo_home = root / "cargo-home"
    # Copy only authenticated dependency archives/databases; extracted sources and
    # compiled objects are never shared between builds.
    for relative in ("registry/index", "registry/cache", "git/db"):
        shutil.copytree(paths["cargo_dependency_home"] / relative, cargo_home / relative, symlinks=False)
    remap = " ".join([f"--remap-path-prefix={root}=/build",
                      f"--remap-path-prefix={cargo_home}=/cargo",
                      f"--remap-path-prefix={fc}=/firecracker"])
    cflags = f"{prefix_map(root)} -ffile-prefix-map={cargo_home}=/cargo -ffile-prefix-map={fc}=/firecracker"
    renv = dict(env,
                PATH=f"{paths['cargo_dependency_home']}/bin:{env['PATH']}",
                CARGO_HOME=str(cargo_home), RUSTUP_HOME=str(paths["rustup_home"]), RUSTUP_TOOLCHAIN=lock["rust_toolchain"],
                CARGO_TARGET_DIR=str(root / "cargo-target"), CARGO_INCREMENTAL="0",
                # -Wl,-S: keep build-root object paths out of the LC_UUID computation.
                RUSTFLAGS=f"-Ccodegen-units=1 -Clink-arg=-Wl,-S {remap}",
                CFLAGS_aarch64_apple_darwin=cflags, CC_aarch64_apple_darwin="/usr/bin/clang",
                GLIB_PREFIX=str(destdir / NATIVE_PREFIX.lstrip("/")))
    log.run(["cargo", "build", "--locked", "--offline", "-p", "firecracker", "--release",
             "--target", "aarch64-apple-darwin"], renv, cwd=fc)
    return package_firecracker(root, renv, log, destdir)


def dependencies(path: pathlib.Path) -> list[str]:
    lines = output("/usr/bin/otool", "-L", path).splitlines()[1:]
    return [line.strip().split(" (compatibility version")[0] for line in lines if line.strip()]


def package_firecracker(root: pathlib.Path, env: dict, log: Log, destdir: pathlib.Path) -> pathlib.Path:
    """Mirror experiments/hvf/distribution/package.py for a DESTDIR-installed logical prefix."""
    fc = root / "src/firecracker-macos"
    out = root / "out/firecracker-package"
    executable = out / "bin/firecracker"
    executable.parent.mkdir(parents=True)
    (out / "lib").mkdir()
    shutil.copyfile(root / "cargo-target/aarch64-apple-darwin/release/firecracker", executable)
    executable.chmod(0o755)
    log.run(["/usr/bin/strip", "-S", executable], env)
    pending, binaries = [executable], []
    while pending:
        binary = pending.pop()
        binaries.append(binary)
        for dependency in dependencies(binary):
            if dependency.startswith(("/usr/lib/", "/System/Library/")):
                continue
            if not dependency.startswith(NATIVE_PREFIX + "/lib/"):
                raise SystemExit(f"unexpected dependency in {binary}: {dependency}")
            original = destdir / dependency.lstrip("/")
            library = out / "lib" / original.name
            if not library.exists():
                shutil.copyfile(original, library)
                library.chmod(0o644)
                pending.append(library)
            replacement = ("@loader_path/../lib/" if binary == executable else "@loader_path/") + library.name
            log.run(["/usr/bin/install_name_tool", "-change", dependency, replacement, binary], env)
        if binary != executable:
            log.run(["/usr/bin/install_name_tool", "-id", "@loader_path/" + binary.name, binary], env)
    unsigned = {}
    for binary in binaries:
        subprocess.run(["/usr/bin/codesign", "--remove-signature", str(binary)], check=True, capture_output=True)
        unsigned[binary.relative_to(out).as_posix()] = sha256_file(binary)
    for binary in binaries:
        command = ["/usr/bin/codesign", "--force", "--sign", "-", "--timestamp=none"]
        if binary == executable:
            command += ["--entitlements", str(fc / "experiments/hvf/entitlements.plist")]
        log.run([*command, binary], env)
        log.run(["/usr/bin/codesign", "--verify", "--strict", binary], env)
        for dependency in dependencies(binary):
            if not dependency.startswith(("/usr/lib/", "/System/Library/", "@loader_path/")):
                raise SystemExit(f"non-relocatable dependency: {dependency}")
    licenses = out / "licenses"
    licenses.mkdir()
    saved = dict(os.environ)
    try:
        os.environ.clear()
        os.environ.update(env)
        sys.path.insert(0, str(fc / "experiments/hvf/distribution"))
        import rust_notices  # noqa: E402  (loaded from the frozen snapshot)
        rust_dependencies = rust_notices.collect(licenses / "rust")
    finally:
        sys.path.pop(0)
        os.environ.clear()
        os.environ.update(saved)
    shutil.copyfile(fc / "LICENSE", licenses / "firecracker-Apache-2.0.txt")
    for notice in ("NOTICE", "THIRD-PARTY"):
        shutil.copyfile(fc / notice, licenses / ("firecracker-" + notice))
    shutil.copyfile(fc / "src/hvf-vmm/vendor/libslirp/COPYRIGHT", licenses / "libslirp-COPYRIGHT")
    shutil.copytree(root / "native/glib-2.88.3/LICENSES", licenses / "glib")
    sources = out / "sources"
    shutil.copytree(fc / "experiments/hvf/build/distribution/downloads", sources)
    for archive, member, name in (
        ("proxy-libintl-0.5.tar.gz", "proxy-libintl-0.5/COPYING", "proxy-libintl-COPYING"),
        ("pcre2-10.46.tar.bz2", "pcre2-10.46/COPYING", "pcre2-COPYING"),
        ("libffi-3.5.2.tar.gz", "libffi-3.5.2/LICENSE", "libffi-LICENSE"),
    ):
        with tarfile.open(sources / archive) as source_archive:
            extracted = source_archive.extractfile(member)
            assert extracted is not None
            with extracted:
                (licenses / name).write_bytes(extracted.read())
    shutil.copytree(fc / "src/hvf-vmm/vendor", sources / "network")
    shutil.copyfile(fc / "experiments/hvf/distribution/sources.json", out / "native-sources.json")
    shutil.copyfile(fc / "Cargo.lock", out / "Cargo.lock")
    shutil.copyfile(fc / "experiments/hvf/distribution/README.md", out / "README.md")
    snapshot = root / "snapshot-info.json"
    info = json.loads(snapshot.read_text())
    manifest = {"format_version": 1, "architecture": "arm64", "minimum_macos": "26.0",
                "api": "1.0", "signing": "ad-hoc", "notarized": False,
                "rust": env["RUSTUP_TOOLCHAIN"], "rust_dependencies": rust_dependencies,
                "sdk": output("/usr/bin/xcrun", "--show-sdk-version"),
                "compiler": output("/usr/bin/clang", "--version"),
                "git_commit": info["firecracker_head"], "dirty": info["firecracker_dirty"],
                "source_snapshot_tree_digest": info["tree_digest"],
                "native_prefix": NATIVE_PREFIX,
                "unsigned_macho_sha256": unsigned,
                # Set only by an independent comparison, never by a single build.
                "reproducibility_verified": False}
    (out / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    for path in sorted(out.rglob("*")):
        if path.is_dir():
            path.chmod(0o755)
        elif path.is_file() and path not in binaries:
            path.chmod(0o644)
    files = sorted(p for p in out.rglob("*") if p.is_file())
    (out / "SHA256SUMS").write_text("".join(f"{sha256_file(p)}  {p.relative_to(out).as_posix()}\n" for p in files))
    return out


def cmd_build(args) -> None:
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        raise SystemExit("reproducible build requires macOS ARM64")
    lock_file = args.lock.resolve()
    lock = json.loads(lock_file.read_text())
    drift = check_lock(lock)
    if drift and not args.allow_toolchain_drift:
        raise SystemExit("toolchain differs from lock (use a matching environment):\n  " + "\n  ".join(drift))
    snapshot = args.snapshot.resolve()
    digest = verify_snapshot(snapshot)
    root = args.work.resolve()
    if root.exists():
        raise SystemExit(f"build root already exists (refusing to reuse objects): {root}")
    for ancestor in (root, *root.parents):
        if (ancestor / ".git").exists():
            raise SystemExit(f"build root must be outside any git checkout: {ancestor}")
    root.mkdir(parents=True)
    log = Log(root / "build.log")
    epoch = int((snapshot / "source-date-epoch.txt").read_text()) if (snapshot / "source-date-epoch.txt").exists() \
        else int(args.source_date_epoch)
    jerboa_head = (snapshot / "jerboa-HEAD.txt").read_text().strip()
    info = {
        "tree_digest": digest,
        "jerboa_head": jerboa_head,
        "jerboa_dirty": bool((snapshot / "jerboa-status.txt").read_text().strip()),
        "firecracker_head": (snapshot / "firecracker-HEAD.txt").read_text().strip(),
        "firecracker_dirty": bool((snapshot / "firecracker-status.txt").read_text().strip()),
    }
    (root / "snapshot-info.json").write_text(json.dumps(info, indent=2, sort_keys=True) + "\n")
    safe_extract_snapshot(snapshot / "source-snapshot.tar", root / "src")
    for required in ("jerboa/go.sum", "jerboa/kernel/vendor/lwip/.vendored", "firecracker-macos/Cargo.lock",
                     "firecracker-macos/experiments/hvf/distribution/sources.json",
                     "firecracker-macos/experiments/hvf/entitlements.plist", "firecracker-macos/.cargo/config.toml"):
        if not (root / "src" / required).is_file():
            raise SystemExit(f"snapshot lacks required input: {required}")
    toolbin = root / "toolbin"
    toolbin.mkdir()
    paths = lock_paths(lock)
    for tool in ("as", "ld", "objcopy", "objdump", "strip"):
        (toolbin / f"aarch64-elf-{tool}").symlink_to(paths["aarch64_elf_binutils"] / f"aarch64-elf-{tool}")
    env = base_env(root, lock, epoch, toolbin)

    kernel = build_kernel(root, env, log, jerboa_head, digest)
    x86 = kernel["tools_dir"] / "x86"
    x86.mkdir()
    for name, expected in sorted(lock["fingerprint"]["x86_tools"].items()):
        shutil.copyfile(paths["x86_tools"] / name, x86 / name)
        if sha256_file(x86 / name) != expected:
            raise SystemExit(f"pinned x86 tool mismatch: {name}")
    jerboa_dir = build_go(root, env, log, lock, args.version)
    destdir = build_glib(root, env, log)
    fc_package = build_firecracker(root, env, log, lock, destdir)

    recipe = {name: sha256_file(path) for name, path in (
        ("scripts/build-macos-reproducible.py", SCRIPT),
        ("scripts/package-macos-distribution.py", SCRIPT.with_name("package-macos-distribution.py")),
        ("lock", lock_file))}
    provenance = {"source_snapshot_tree_digest": digest, "recipe_sha256": recipe,
                  "jerboa_dirty": info["jerboa_dirty"], "firecracker_git_commit": info["firecracker_head"],
                  "firecracker_dirty": info["firecracker_dirty"], "toolchain_matches_lock": not drift,
                  "x86_tools": "pinned prebuilt input, not rebuilt"}
    (root / "provenance.json").write_text(json.dumps(provenance, indent=2, sort_keys=True) + "\n")
    dist = root / "out/distribution"
    log.run([sys.executable, SCRIPT.with_name("package-macos-distribution.py"),
             "--jerboa-dir", jerboa_dir, "--tools-dir", kernel["tools_dir"], "--firecracker-package", fc_package,
             "--output-dir", dist, "--version", args.version, "--source-date-epoch", str(epoch),
             "--source-root", root / "src/jerboa", "--jerboa-git-commit", jerboa_head,
             "--provenance-json", root / "provenance.json"], env)
    bundle = dist / f"jerboa-{args.version}-macos-arm64"
    archive = dist / f"{bundle.name}.tar.gz"
    record = {
        "format_version": 1,
        "version": args.version,
        "build_root": str(root),
        "toolchain_drift": drift,
        "snapshot": info,
        "recipe_sha256": recipe,
        "archive_sha256": sha256_file(archive),
        "components": {
            "jerboa": inventory(jerboa_dir),
            "tools": inventory(kernel["tools_dir"]),
            "firecracker-package": inventory(fc_package),
            "bundle": inventory(bundle),
        },
    }
    (root / "build-record.json").write_text(json.dumps(record, indent=2, sort_keys=True) + "\n")
    print(json.dumps({"build_root": str(root), "archive": str(archive), "archive_sha256": record["archive_sha256"]}))


# ------------------------------------------------------------------------ compare

def compare_records(first: dict, second: dict) -> dict:
    differences = []
    for component in sorted(set(first["components"]) | set(second["components"])):
        a, b = first["components"].get(component, {}), second["components"].get(component, {})
        for name in sorted(set(a) | set(b)):
            left, right = a.get(name), b.get(name)
            if left is None or right is None:
                differences.append({"component": component, "file": name, "kind": "missing",
                                    "first": left is not None, "second": right is not None})
                continue
            for key in ("sha256", "unsigned_sha256", "mode"):
                if left.get(key) != right.get(key):
                    differences.append({"component": component, "file": name, "kind": key,
                                        "first": left.get(key), "second": right.get(key)})
    same_inputs = (first["snapshot"] == second["snapshot"] and first["recipe_sha256"] == second["recipe_sha256"]
                   and first["version"] == second["version"])
    distinct_roots = first["build_root"] != second["build_root"]
    counts = {c: len(first["components"].get(c, {})) for c in first["components"]}
    macho = sorted({f"{c}/{n}" for c in first["components"] for n, e in first["components"][c].items() if "unsigned_sha256" in e})
    return {
        "same_snapshot_and_recipe": same_inputs,
        "distinct_build_roots": distinct_roots,
        "toolchain_drift": {"first": first["toolchain_drift"], "second": second["toolchain_drift"]},
        "archive_sha256": {"first": first["archive_sha256"], "second": second["archive_sha256"]},
        "archive_equal": first["archive_sha256"] == second["archive_sha256"],
        "files_compared": counts,
        "macho_files": macho,
        "differences": differences,
        "reproducible": (same_inputs and distinct_roots and not differences
                         and first["archive_sha256"] == second["archive_sha256"]
                         and not first["toolchain_drift"] and not second["toolchain_drift"]),
        "scope": ("signed ad-hoc bytes (sha256) and pre-signature Mach-O bytes (unsigned_sha256) compared per file; "
                  "valid only for the locked toolchain/host environment"),
    }


def cmd_compare(args) -> None:
    first = json.loads((args.first / "build-record.json").read_text())
    second = json.loads((args.second / "build-record.json").read_text())
    report = compare_records(first, second)
    text = json.dumps(report, indent=2, sort_keys=True) + "\n"
    if args.output:
        args.output.write_text(text)
    print(text, end="")
    if not report["reproducible"]:
        raise SystemExit(1)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    snap = sub.add_parser("snapshot")
    snap.add_argument("--pack", type=pathlib.Path, default=PACK, help="directory containing jerboa and firecracker-macos")
    snap.add_argument("--output", type=pathlib.Path, required=True)
    snap.set_defaults(func=cmd_snapshot)
    lock = sub.add_parser("lock")
    lock.add_argument("--output", type=pathlib.Path, required=True)
    lock.set_defaults(func=cmd_lock)
    build = sub.add_parser("build")
    build.add_argument("--snapshot", type=pathlib.Path, required=True)
    build.add_argument("--work", type=pathlib.Path, required=True, help="new build root outside any git checkout")
    build.add_argument("--version", required=True)
    build.add_argument("--lock", type=pathlib.Path, default=DEFAULT_LOCK)
    build.add_argument("--source-date-epoch", type=int, default=0)
    build.add_argument("--allow-toolchain-drift", action="store_true",
                       help="build anyway; the record marks it and compare will refuse to call it reproducible")
    build.set_defaults(func=cmd_build)
    compare = sub.add_parser("compare")
    compare.add_argument("first", type=pathlib.Path)
    compare.add_argument("second", type=pathlib.Path)
    compare.add_argument("--output", type=pathlib.Path)
    compare.set_defaults(func=cmd_compare)
    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
