"""Merge verified workflow package artifacts into the public release index.

Published versions are immutable. A (name, version, platform) already in the
index is never replaced: a rebuild with the same archive or the same
content_digest is skipped, and one with different content is refused, because
a client that cached the old archive would no longer match the index.

Prints the file names of new archives, one per line, for the caller to upload.
"""
import hashlib
import json
import pathlib
import shutil
import sys

ALLOWED = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"


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
    index_path.write_text(json.dumps(index, indent=2) + "\n")
    for filename in added:
        print(filename)


if __name__ == "__main__":
    main()
