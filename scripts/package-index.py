"""Merge verified workflow package artifacts into the public release index."""
import hashlib
import json
import pathlib
import shutil
import sys

source, destination = map(pathlib.Path, sys.argv[1:3])
repository = sys.argv[3]
index_path = destination / "packages.json"
index = json.loads(index_path.read_text()) if index_path.exists() else {"packages": {}}
metadata = sorted(source.rglob("meta.json"))
if not metadata:
    raise SystemExit("No package metadata found")
for meta_path in metadata:
    package = json.loads(meta_path.read_text())
    archive = meta_path.parent / "files.tar.gz"
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    if digest != package["sha256"] or archive.stat().st_size != package["size"]:
        raise SystemExit(f"Archive verification failed: {archive}")
    name, version = package["name"], package["version"]
    if any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-" for c in name + version):
        raise SystemExit("Invalid package name/version")
    filename = f"{name}-{version}.tar.gz"
    shutil.copyfile(archive, destination / filename)
    package["url"] = f"https://github.com/{repository}/releases/download/pkg-index/{filename}"
    versions = index["packages"].setdefault(name, [])
    versions[:] = [p for p in versions if p["version"] != version]
    versions.append(package)
index_path.write_text(json.dumps(index, indent=2) + "\n")
