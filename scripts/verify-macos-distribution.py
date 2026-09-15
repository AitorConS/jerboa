#!/usr/bin/env python3
"""Verify a staged or archived Jerboa macOS ARM64 distribution."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import platform
import plistlib
import shutil
import stat
import subprocess
import tarfile
import tempfile


MACHO = ("bin/jerboa", "bin/jerboad", "bin/firecracker", "bin/tools/mkfs", "bin/tools/dump", "lib/libglib-2.0.0.dylib", "lib/libintl.8.dylib")
ALLOWED_PREFIXES = ("/usr/lib/", "/System/Library/", "@loader_path/")


def run(*argv: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(argv, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=check)


def sha256(path: pathlib.Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def safe_extract(archive: pathlib.Path, target: pathlib.Path) -> pathlib.Path:
    with tarfile.open(archive, "r:gz") as source:
        members = source.getmembers()
        names = set()
        for member in members:
            if not member.name:
                raise SystemExit("archive contains an empty member path")
            path = pathlib.PurePosixPath(member.name)
            normalized = path.as_posix()
            if (path.is_absolute() or not path.parts or ".." in path.parts
                    or member.name.rstrip("/") != normalized):
                raise SystemExit(f"unsafe archive member: {member.name}")
            if normalized in names:
                raise SystemExit(f"duplicate archive member: {member.name}")
            names.add(normalized)
            if not (member.isdir() or member.isreg()):
                raise SystemExit(f"archive member is not a regular file/directory: {member.name}")
        roots = {pathlib.PurePosixPath(member.name).parts[0] for member in members if member.name}
        if len(roots) != 1:
            raise SystemExit("archive must have exactly one top-level directory")
        # Extract manually after validating the complete inventory. This avoids
        # legacy extractall behavior and guarantees that no special node/link is
        # ever created, even on the older system Python shipped with macOS.
        for member in sorted((item for item in members if item.isdir()), key=lambda item: len(pathlib.PurePosixPath(item.name).parts)):
            destination = target.joinpath(*pathlib.PurePosixPath(member.name).parts)
            destination.mkdir(parents=True, exist_ok=False)
            destination.chmod(member.mode & 0o777)
        for member in (item for item in members if item.isreg()):
            destination = target.joinpath(*pathlib.PurePosixPath(member.name).parts)
            destination.parent.mkdir(parents=True, exist_ok=True)
            extracted = source.extractfile(member)
            if extracted is None:
                raise SystemExit(f"cannot read archive member: {member.name}")
            with extracted, destination.open("xb") as output:
                shutil.copyfileobj(extracted, output)
            destination.chmod(member.mode & 0o777)
    return target / roots.pop()


def validate_regular_tree(root: pathlib.Path) -> None:
    try:
        mode = root.lstat().st_mode
    except FileNotFoundError:
        raise SystemExit(f"bundle does not exist: {root}")
    if not stat.S_ISDIR(mode):
        raise SystemExit(f"bundle must be a real directory (no symlinks): {root}")
    for current, directories, files in os.walk(root, followlinks=False):
        for name in directories:
            path = pathlib.Path(current) / name
            if not stat.S_ISDIR(path.lstat().st_mode):
                raise SystemExit(f"bundle contains a symlink/special directory: {path}")
        for name in files:
            path = pathlib.Path(current) / name
            if not stat.S_ISREG(path.lstat().st_mode):
                raise SystemExit(f"bundle contains a symlink/special file: {path}")


def compare_installer_payload(bundle: pathlib.Path, payload: pathlib.Path) -> int:
    """Prove that the installer payload contains exactly the verified bundle."""
    validate_regular_tree(payload)
    installed = payload / "usr/local/libexec/jerboa"
    validate_regular_tree(installed)
    bundle_files = {
        path.relative_to(bundle).as_posix(): path
        for path in bundle.rglob("*") if path.is_file() and not path.is_symlink()
    }
    payload_files = {
        path.relative_to(installed).as_posix(): path
        for path in installed.rglob("*") if path.is_file() and not path.is_symlink()
    }
    if bundle_files.keys() != payload_files.keys():
        raise SystemExit(f"installer payload inventory differs from bundle: {sorted(bundle_files.keys() ^ payload_files.keys())}")
    for relative, source in bundle_files.items():
        packaged = payload_files[relative]
        if sha256(source) != sha256(packaged):
            raise SystemExit(f"installer payload bytes differ from bundle: {relative}")
        if stat.S_IMODE(source.stat().st_mode) != stat.S_IMODE(packaged.stat().st_mode):
            raise SystemExit(f"installer payload mode differs from bundle: {relative}")
    all_payload_files = {
        path.relative_to(payload).as_posix()
        for path in payload.rglob("*") if path.is_file() and not path.is_symlink()
    }
    expected_payload_files = {f"usr/local/libexec/jerboa/{relative}" for relative in bundle_files}
    if all_payload_files != expected_payload_files:
        raise SystemExit(f"installer contains files outside verified bundle: {sorted(all_payload_files ^ expected_payload_files)}")
    return len(bundle_files)


def verify_checksums(root: pathlib.Path) -> int:
    count = 0
    seen = set()
    for number, raw in enumerate((root / "SHA256SUMS").read_text().splitlines(), 1):
        try:
            expected, relative = raw.split(None, 1)
        except ValueError as error:
            raise SystemExit(f"invalid checksum entry at line {number}") from error
        relative = relative.lstrip("* ")
        relative_path = pathlib.PurePosixPath(relative)
        if (relative_path.is_absolute() or not relative_path.parts or ".." in relative_path.parts
                or relative_path.as_posix() != relative or len(expected) != 64
                or any(character not in "0123456789abcdefABCDEF" for character in expected)
                or relative in seen or relative == "SHA256SUMS"):
            raise SystemExit(f"invalid checksum entry at line {number}")
        seen.add(relative)
        path = root / relative
        try:
            path.resolve().relative_to(root.resolve())
        except ValueError as error:
            raise SystemExit(f"checksum path escapes bundle: {relative}") from error
        if not stat.S_ISREG(path.lstat().st_mode) or sha256(path) != expected.lower():
            raise SystemExit(f"checksum mismatch: {relative}")
        count += 1
    actual = {path.relative_to(root).as_posix() for path in root.rglob("*") if path.is_file() and not path.is_symlink()}
    if actual - {"SHA256SUMS"} != seen:
        raise SystemExit(f"checksum inventory mismatch: {sorted((actual - {'SHA256SUMS'}) ^ seen)}")
    return count


def dependencies(path: pathlib.Path) -> list[str]:
    lines = run("otool", "-L", str(path)).stdout.splitlines()[1:]
    return [line.strip().split(" (compatibility", 1)[0] for line in lines if line.strip()]


def signature_kind(path: pathlib.Path) -> str:
    output = run("codesign", "-dv", "--verbose=4", str(path)).stdout
    if "Signature=adhoc" in output or "flags=0x2(adhoc)" in output or "flags=0x20002(adhoc" in output:
        return "adhoc"
    if "Authority=Developer ID Application:" in output:
        return "developer-id"
    return "unknown"


def entitlements(path: pathlib.Path) -> dict:
    result = run("codesign", "-d", "--entitlements", ":-", str(path), check=False)
    start = result.stdout.find("<?xml")
    if start < 0:
        return {}
    return plistlib.loads(result.stdout[start:].encode())


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("bundle", type=pathlib.Path)
    parser.add_argument("--mode", choices=("adhoc", "release"), default="adhoc")
    parser.add_argument("--installer-pkg", type=pathlib.Path)
    args = parser.parse_args()
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        raise SystemExit("verification requires macOS ARM64")

    with tempfile.TemporaryDirectory(prefix="jerboa-dist-verify-") as temporary:
        if args.bundle.is_symlink():
            raise SystemExit(f"bundle path must not be a symlink: {args.bundle}")
        root = (safe_extract(args.bundle, pathlib.Path(temporary)) if args.bundle.is_file()
                else args.bundle).resolve()
        validate_regular_tree(root)
        manifest = json.loads((root / "distribution-manifest.json").read_text())
        if manifest["platform"] != "darwin/arm64" or manifest["minimum_macos"] != "26.0":
            raise SystemExit("unexpected distribution platform/minimum macOS")
        if manifest.get("actual_signing") == "developer-id":
            metadata = root / "share/firecracker"
            if (metadata / "manifest.json").exists() or (metadata / "SHA256SUMS").exists():
                raise SystemExit("signed bundle contains ambiguous pre-signing Firecracker metadata")
            for name in ("manifest.upstream-adhoc.json", "SHA256SUMS.upstream-adhoc", "SIGNING-NOTICE.json"):
                if not (metadata / name).is_file():
                    raise SystemExit(f"signed bundle is missing marked upstream metadata: {name}")
            notice = json.loads((metadata / "SIGNING-NOTICE.json").read_text())
            if notice.get("authoritative_signed_inventory") != "../../SHA256SUMS":
                raise SystemExit("signed bundle has an invalid Firecracker signing notice")
        count = verify_checksums(root)
        for relative in MACHO:
            path = root / relative
            if "arm64" not in run("file", "-b", str(path)).stdout or "Mach-O" not in run("file", "-b", str(path)).stdout:
                raise SystemExit(f"not Mach-O ARM64: {relative}")
            run("codesign", "--verify", "--strict", "--verbose=2", str(path))
            kind = signature_kind(path)
            if args.mode == "release" and kind != "developer-id":
                raise SystemExit(f"release requires Developer ID signature: {relative} ({kind})")
            if args.mode == "adhoc" and kind not in ("adhoc", "developer-id"):
                raise SystemExit(f"unrecognized signature: {relative}")
            for dependency in dependencies(path):
                if not dependency.startswith(ALLOWED_PREFIXES):
                    raise SystemExit(f"non-relocatable dependency in {relative}: {dependency}")
                if dependency.startswith("@loader_path/"):
                    resolved = (path.parent / dependency.removeprefix("@loader_path/")).resolve()
                    try:
                        resolved.relative_to(root)
                    except ValueError as error:
                        raise SystemExit(f"dependency escapes bundle in {relative}: {dependency}") from error
                    if not resolved.is_file():
                        raise SystemExit(f"missing bundled dependency in {relative}: {dependency}")
        expected_entitlements = {"com.apple.security.hypervisor": True}
        if entitlements(root / "bin/firecracker") != expected_entitlements:
            raise SystemExit("Firecracker must carry exactly the Hypervisor entitlement")
        for relative in ("bin/jerboa", "bin/jerboad", "bin/tools/mkfs", "bin/tools/dump"):
            if entitlements(root / relative):
                raise SystemExit(f"unexpected entitlements: {relative}")

        # Relocated, isolated installation smoke. These commands do not start a daemon or VM.
        for relative in ("bin/jerboa", "bin/jerboad", "bin/firecracker"):
            result = run(str(root / relative), "--version")
            if not result.stdout.strip():
                raise SystemExit(f"empty --version output: {relative}")
        if args.mode == "release":
            if (not args.installer_pkg or args.installer_pkg.is_symlink()
                    or not args.installer_pkg.is_file()
                    or not stat.S_ISREG(args.installer_pkg.lstat().st_mode)):
                raise SystemExit("release verification requires --installer-pkg")
            signature = run("pkgutil", "--check-signature", str(args.installer_pkg)).stdout
            if "Developer ID Installer:" not in signature:
                raise SystemExit("installer is not signed by Developer ID Installer")
            run("xcrun", "stapler", "validate", str(args.installer_pkg))
            run("spctl", "--assess", "--type", "install", "--verbose=2", str(args.installer_pkg))
            expanded = pathlib.Path(temporary) / "expanded-pkg"
            run("pkgutil", "--expand-full", str(args.installer_pkg), str(expanded))
            if (expanded / "Scripts").exists():
                raise SystemExit("installer scripts are not permitted")
            compare_installer_payload(root, expanded / "Payload")
        print(json.dumps({"result": "PASS", "mode": args.mode, "files": count, "bundle": str(root)}, sort_keys=True))


if __name__ == "__main__":
    main()
