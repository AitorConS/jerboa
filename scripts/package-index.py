"""Merge verified workflow package artifacts into the public release index.

Published versions are immutable. A (name, version, platform) already in the
index is never replaced: a rebuild with the same archive or the same
content_digest is skipped, and one with different content is refused, because
a client that cached the old archive would no longer match the index.

The index carries a purpose, a publication time and a serial that grows by
one with every publication that adds an entry; clients refuse an index whose
serial is below the newest they accepted. When PKG_INDEX_MIN_SERIAL is set
(the serial of the last publication, recorded where release-asset writers
cannot change it; the publish workflow always sets it, 0 standing for "no
serial published yet"), the base index must carry exactly that serial. An
older base is a superseded index restored on the release, which is not
extended and signed again. A newer one means the record was not raised after
the last publication: the run stops and says how to raise it, so the record
cannot silently fall behind. A base without a serial (the format before
serials) counts as serial 0.

Prints the file names of new archives, one per line, for the caller to upload.
"""
import datetime
import hashlib
import json
import os
import pathlib
import shutil
import sys

ALLOWED = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"
PURPOSE = "jerboa-package-index"


def check_base(index, min_serial):
    """Refuse a base index that is not a package index or is not the recorded one.

    min_serial is None when no record is kept (local use)."""
    serial = index.get("serial", 0)
    if "serial" in index or "purpose" in index:
        if index.get("purpose") != PURPOSE:
            raise SystemExit(f"Base index purpose is {index.get('purpose')!r}, not {PURPOSE!r}")
        if not isinstance(serial, int) or isinstance(serial, bool) or serial < 1:
            raise SystemExit(f"Base index has an invalid serial {serial!r}")
    if min_serial is None:
        return serial
    if serial < min_serial:
        raise SystemExit(
            f"Base index serial {serial} is older than the last publication ({min_serial}); "
            "restore the newest packages.json on the release instead of extending an older one")
    if serial > min_serial:
        raise SystemExit(
            f"Base index serial {serial} is newer than PKG_INDEX_MIN_SERIAL ({min_serial}): the variable was not "
            f"raised after the last publication. Check that the summary of the last \"Package Build\" run that "
            f"published shows serial {serial}; if it does, run\n"
            f"  gh variable set PKG_INDEX_MIN_SERIAL --body {serial}\n"
            "and run this workflow again. If no run published that serial, the index on the release was "
            "replaced: do not extend it. Never lower the variable.")
    return serial


def same_content(old, new):
    if old.get("sha256") == new["sha256"]:
        return True
    return bool(old.get("content_digest")) and old.get("content_digest") == new.get("content_digest")


def archive_name(name, version, platform):
    suffix = "-" + platform.replace("/", "-") if platform else ""
    return f"{name}-{version}{suffix}.tar.gz"


def check_urls(index, repository):
    """Every entry must point at its own archive on this repository's index release."""
    base = f"https://github.com/{repository}/releases/download/pkg-index/"
    for name, versions in index["packages"].items():
        for p in versions:
            if p.get("url") != base + archive_name(name, p["version"], p.get("platform", "")):
                raise SystemExit(f"Index entry {name}:{p['version']} points outside the package index")


def main():
    source, destination = map(pathlib.Path, sys.argv[1:3])
    repository = sys.argv[3]
    index_path = destination / "packages.json"
    index = json.loads(index_path.read_text()) if index_path.exists() else {"packages": {}}
    floor = (os.environ.get("PKG_INDEX_MIN_SERIAL") or "").strip()
    min_serial = int(floor) if floor else None
    serial = check_base(index, min_serial)
    check_urls(index, repository)
    metadata = sorted(source.rglob("meta.json"))
    if not metadata:
        raise SystemExit("No package metadata found")

    added, conflicts = [], []
    for meta_path in metadata:
        package = json.loads(meta_path.read_text())
        archive = meta_path.parent / "files.tar.gz"
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        if digest != package["sha256"] or archive.stat().st_size != package["size"]:
            raise SystemExit(f"Archive verification failed: {archive}")
        name, version = package["name"], package["version"]
        if any(c not in ALLOWED for c in name + version):
            raise SystemExit("Invalid package name/version")
        platform = package.get("platform", "")
        if platform and platform not in ("linux/arm64", "linux/amd64"):
            raise SystemExit("Invalid package platform")

        versions = index["packages"].setdefault(name, [])
        existing = [p for p in versions if (p["version"], p.get("platform", "")) == (version, platform)]
        if existing:
            if not same_content(existing[0], package):
                conflicts.append(f"{name}:{version} ({platform or 'no platform'})")
            else:
                print(f"{name}:{version} ({platform or 'no platform'}) is already published", file=sys.stderr)
            continue

        filename = archive_name(name, version, platform)
        shutil.copyfile(archive, destination / filename)
        package["url"] = f"https://github.com/{repository}/releases/download/pkg-index/{filename}"
        versions.append(package)
        added.append(filename)

    if conflicts:
        raise SystemExit(
            "Refusing to republish with different content (bump the version instead): " + ", ".join(conflicts))
    if added:
        index["purpose"] = PURPOSE
        index["serial"] = serial + 1
        index["generated"] = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        print(f"Package index serial {index['serial']}", file=sys.stderr)
    index_path.write_text(json.dumps(index, indent=2) + "\n")
    for filename in added:
        print(filename)


if __name__ == "__main__":
    main()
