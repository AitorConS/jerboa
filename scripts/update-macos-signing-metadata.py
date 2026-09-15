#!/usr/bin/env python3
"""Mark upstream ad-hoc Firecracker metadata before Developer ID signing."""

from __future__ import annotations

import hashlib
import json
import os
import pathlib
import stat
import sys


def regular_files(root: pathlib.Path) -> list[pathlib.Path]:
    result = []
    if not stat.S_ISDIR(root.lstat().st_mode):
        raise SystemExit("signed bundle must be a real directory")
    for current, directories, files in os.walk(root, followlinks=False):
        for name in directories:
            path = pathlib.Path(current) / name
            if not stat.S_ISDIR(path.lstat().st_mode):
                raise SystemExit(f"signed bundle contains a symlink/special directory: {path}")
        for name in files:
            path = pathlib.Path(current) / name
            if not stat.S_ISREG(path.lstat().st_mode):
                raise SystemExit(f"signed bundle contains a symlink/special file: {path}")
            result.append(path)
    return sorted(result, key=lambda path: path.relative_to(root).as_posix())


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit(f"Usage: {sys.argv[0]} BUNDLE")
    root = pathlib.Path(sys.argv[1])
    metadata = root / "share/firecracker"
    renames = {
        metadata / "manifest.json": metadata / "manifest.upstream-adhoc.json",
        metadata / "SHA256SUMS": metadata / "SHA256SUMS.upstream-adhoc",
    }
    for source, target in renames.items():
        if not source.is_file() or source.is_symlink() or target.exists():
            raise SystemExit(f"cannot mark upstream signing metadata: {source}")
    for source, target in renames.items():
        source.rename(target)
    notice = {
        "authoritative_signed_inventory": "../../SHA256SUMS",
        "scope": "The renamed upstream files describe the original pre-Developer-ID ad-hoc Firecracker package; Developer ID signing changed Mach-O bytes.",
        "upstream_manifest": "manifest.upstream-adhoc.json",
        "upstream_checksums": "SHA256SUMS.upstream-adhoc",
    }
    (metadata / "SIGNING-NOTICE.json").write_text(json.dumps(notice, indent=2, sort_keys=True) + "\n")
    manifest_path = root / "distribution-manifest.json"
    manifest = json.loads(manifest_path.read_text())
    manifest["actual_signing"] = "developer-id"
    manifest["notarized"] = False
    manifest["upstream_firecracker_metadata_scope"] = "renamed files describe pre-Developer-ID ad-hoc inputs"
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    lines = []
    for path in regular_files(root):
        relative = path.relative_to(root).as_posix()
        if relative == "SHA256SUMS":
            continue
        lines.append(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {relative}\n")
    (root / "SHA256SUMS").write_text("".join(lines))


if __name__ == "__main__":
    main()
