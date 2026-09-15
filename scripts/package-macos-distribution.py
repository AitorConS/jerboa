#!/usr/bin/env python3
"""Build a deterministic Jerboa + Firecracker/HVF macOS ARM64 bundle.

This command only stages already-built inputs. It never signs, installs, submits,
or publishes anything. Run it after all release-candidate binaries are final.
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import os
import pathlib
import shutil
import stat
import subprocess
import tarfile


EXECUTABLES = {
    "bin/jerboa",
    "bin/jerboad",
    "bin/firecracker",
    "bin/tools/mkfs",
    "bin/tools/dump",
}


def digest(path: pathlib.Path) -> str:
    result = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()


def require_file(path: pathlib.Path, label: str) -> None:
    try:
        mode = path.lstat().st_mode
    except FileNotFoundError:
        raise SystemExit(f"missing {label}: {path}")
    if not stat.S_ISREG(mode):
        raise SystemExit(f"{label} must be a regular file (no symlinks): {path}")


def validate_input_tree(root: pathlib.Path, label: str) -> None:
    try:
        root_mode = root.lstat().st_mode
    except FileNotFoundError:
        raise SystemExit(f"missing {label}: {root}")
    if not stat.S_ISDIR(root_mode):
        raise SystemExit(f"{label} must be a real directory (no symlinks): {root}")
    for current, directories, files in os.walk(root, followlinks=False):
        for name in directories:
            path = pathlib.Path(current) / name
            if not stat.S_ISDIR(path.lstat().st_mode):
                raise SystemExit(f"{label} contains a symlink/special directory: {path}")
        for name in files:
            path = pathlib.Path(current) / name
            if not stat.S_ISREG(path.lstat().st_mode):
                raise SystemExit(f"{label} contains a symlink/special file: {path}")


def command_output(*argv: str) -> str:
    return subprocess.check_output(argv, text=True).strip()


def verify_upstream_checksums(package: pathlib.Path) -> None:
    sums = package / "SHA256SUMS"
    require_file(sums, "Firecracker SHA256SUMS")
    for number, raw in enumerate(sums.read_text().splitlines(), 1):
        if not raw.strip():
            continue
        try:
            expected, relative = raw.split(None, 1)
        except ValueError as error:
            raise SystemExit(f"invalid Firecracker SHA256SUMS line {number}") from error
        relative = relative.lstrip("* ")
        relative_path = pathlib.PurePosixPath(relative)
        if (relative_path.is_absolute() or not relative_path.parts or ".." in relative_path.parts
                or relative_path.as_posix() != relative or len(expected) != 64
                or any(character not in "0123456789abcdefABCDEF" for character in expected)):
            raise SystemExit(f"unsafe/invalid Firecracker SHA256SUMS line {number}")
        candidate = package / relative
        require_file(candidate, f"Firecracker checksum member {relative}")
        try:
            candidate.resolve().relative_to(package.resolve())
        except ValueError as error:
            raise SystemExit(f"Firecracker checksum member escapes package: {relative}") from error
        if digest(candidate) != expected.lower():
            raise SystemExit(f"Firecracker checksum mismatch: {relative}")


def load_provenance(path: pathlib.Path) -> dict:
    """Load build provenance, rejecting host paths that would make bundles build-root dependent."""
    require_file(path, "provenance JSON")
    text = path.read_text()
    value = json.loads(text)
    if not isinstance(value, dict):
        raise SystemExit("provenance JSON must be an object")
    for marker in ("/Users/", "/private/", "/tmp/", "/var/folders/", "/home/"):
        if marker in text:
            raise SystemExit(f"provenance JSON contains a host path ({marker}); it must be build-root independent")
    return value


def copy_tree(source: pathlib.Path, target: pathlib.Path) -> None:
    shutil.copytree(source, target, symlinks=True)


def files_under(root: pathlib.Path):
    return sorted(
        (path for path in root.rglob("*") if path.is_file() and not path.is_symlink()),
        key=lambda path: path.relative_to(root).as_posix(),
    )


def write_checksums(root: pathlib.Path) -> None:
    sums = root / "SHA256SUMS"
    lines = []
    for path in files_under(root):
        relative = path.relative_to(root).as_posix()
        if relative == "SHA256SUMS":
            continue
        lines.append(f"{digest(path)}  {relative}\n")
    sums.write_text("".join(lines))


def normalize_modes(root: pathlib.Path) -> None:
    for path in sorted(root.rglob("*")):
        if path.is_symlink():
            continue
        relative = path.relative_to(root).as_posix()
        if path.is_dir():
            path.chmod(0o755)
        elif relative in EXECUTABLES:
            path.chmod(0o755)
        else:
            path.chmod(0o644)


def tar_filter(epoch: int):
    def normalize(info: tarfile.TarInfo) -> tarfile.TarInfo:
        info.uid = 0
        info.gid = 0
        info.uname = "root"
        info.gname = "wheel"
        info.mtime = epoch
        if info.isdir():
            info.mode = 0o755
        elif info.isfile():
            relative = info.name.split("/", 1)[-1]
            info.mode = 0o755 if relative in EXECUTABLES else 0o644
        return info

    return normalize


def create_archive(bundle: pathlib.Path, archive: pathlib.Path, epoch: int) -> None:
    with archive.open("wb") as output:
        with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=epoch, compresslevel=9) as zipped:
            with tarfile.open(fileobj=zipped, mode="w", format=tarfile.PAX_FORMAT) as tar:
                tar.add(bundle, arcname=bundle.name, recursive=True, filter=tar_filter(epoch))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--jerboa-dir", required=True, type=pathlib.Path)
    parser.add_argument("--tools-dir", required=True, type=pathlib.Path)
    parser.add_argument("--firecracker-package", required=True, type=pathlib.Path)
    parser.add_argument("--output-dir", required=True, type=pathlib.Path)
    parser.add_argument("--version", required=True)
    parser.add_argument("--source-date-epoch", required=True, type=int)
    parser.add_argument("--signing", choices=("adhoc", "developer-id"), default="adhoc")
    parser.add_argument("--source-root", type=pathlib.Path, default=pathlib.Path(__file__).resolve().parents[1],
                        help="Jerboa source tree providing LICENSE/NOTICE (default: this checkout)")
    parser.add_argument("--jerboa-git-commit", help="commit recorded in the manifest (default: git rev-parse HEAD)")
    parser.add_argument("--provenance-json", type=pathlib.Path,
                        help="source-build provenance to embed; must not contain build paths or times")
    args = parser.parse_args()

    if not args.version or "/" in args.version or args.source_date_epoch < 0:
        raise SystemExit("version must be non-empty/path-safe and source date epoch non-negative")
    if args.output_dir.exists():
        raise SystemExit(f"output already exists (refusing to overwrite): {args.output_dir}")

    validate_input_tree(args.jerboa_dir, "Jerboa input")
    validate_input_tree(args.tools_dir, "tools input")
    validate_input_tree(args.firecracker_package, "Firecracker input")
    for name in ("jerboa", "jerboad"):
        require_file(args.jerboa_dir / name, name)
    for name in ("mkfs", "dump", "kernel.img", "boot.img", "platform.txt", "kernel-version.txt"):
        require_file(args.tools_dir / name, f"tool {name}")
    for name in ("bin/firecracker", "lib/libglib-2.0.0.dylib", "lib/libintl.8.dylib", "manifest.json"):
        require_file(args.firecracker_package / name, f"Firecracker {name}")
    verify_upstream_checksums(args.firecracker_package)
    provenance = load_provenance(args.provenance_json) if args.provenance_json else None
    provenance_commit = args.jerboa_git_commit or command_output("git", "rev-parse", "HEAD")
    if len(provenance_commit) != 40 or any(c not in "0123456789abcdef" for c in provenance_commit):
        raise SystemExit(f"invalid Jerboa git commit: {provenance_commit}")

    args.output_dir.mkdir(parents=True)
    bundle = args.output_dir / f"jerboa-{args.version}-macos-arm64"
    (bundle / "bin").mkdir(parents=True)
    shutil.copy2(args.jerboa_dir / "jerboa", bundle / "bin/jerboa")
    shutil.copy2(args.jerboa_dir / "jerboad", bundle / "bin/jerboad")
    copy_tree(args.tools_dir, bundle / "bin/tools")
    shutil.copy2(args.firecracker_package / "bin/firecracker", bundle / "bin/firecracker")
    copy_tree(args.firecracker_package / "lib", bundle / "lib")
    metadata = bundle / "share/firecracker"
    metadata.mkdir(parents=True)
    for entry in sorted(args.firecracker_package.iterdir(), key=lambda path: path.name):
        if entry.name in ("bin", "lib"):
            continue
        target = metadata / entry.name
        copy_tree(entry, target) if entry.is_dir() else shutil.copy2(entry, target)
    for name in ("LICENSE", "NOTICE"):
        require_file(args.source_root / name, f"source {name}")
        shutil.copy2(args.source_root / name, bundle / name)

    manifest = {
        "format_version": 1,
        "name": "jerboa-macos-firecracker",
        "version": args.version,
        "platform": "darwin/arm64",
        "minimum_macos": "26.0",
        "source_date_epoch": args.source_date_epoch,
        "jerboa_git_commit": provenance_commit,
        "requested_signing": args.signing,
        "notarized": False,
        "inputs": {
            "jerboa_sha256": digest(args.jerboa_dir / "jerboa"),
            "jerboad_sha256": digest(args.jerboa_dir / "jerboad"),
            "firecracker_manifest_sha256": digest(args.firecracker_package / "manifest.json"),
            "firecracker_sha256s_sha256": digest(args.firecracker_package / "SHA256SUMS"),
        },
        "reproducibility_scope": "deterministic staging/archive for identical input bytes; reused inputs are not rebuilt",
    }
    if provenance is not None:
        manifest["source_build"] = provenance
        manifest["reproducibility_scope"] = (
            "compiled from the recorded source snapshot with the pinned toolchain lock; bit-for-bit equality "
            "is established only by comparing independent builds in that locked environment")
    (bundle / "distribution-manifest.json").write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    normalize_modes(bundle)
    write_checksums(bundle)
    normalize_modes(bundle)
    archive = args.output_dir / f"{bundle.name}.tar.gz"
    create_archive(bundle, archive, args.source_date_epoch)
    (args.output_dir / f"{archive.name}.sha256").write_text(f"{digest(archive)}  {archive.name}\n")
    print(archive)


if __name__ == "__main__":
    main()
