"""Check public artifacts against the locally signed manifest after publication.

This validates distribution bytes and CLI/daemon versions. Runtime coverage is
reported separately by the integration jobs; this is not a full VM audit.
"""
import hashlib
import json
import pathlib
import subprocess
import sys
import tarfile
import tempfile
import urllib.request


def urlopen(url, timeout):
    # releases.jerboa.dev sits behind Cloudflare, which 403s the default
    # Python-urllib User-Agent. Send a descriptive one so the fetch is allowed.
    request = urllib.request.Request(url, headers={"User-Agent": "jerboa-release-verify/1.0"})
    return urllib.request.urlopen(request, timeout=timeout)


manifest_path, base, expected = sys.argv[1:4]
manifest_bytes = pathlib.Path(manifest_path).read_bytes()
with urlopen(base.rstrip("/") + "/channels/stable.json", timeout=60) as response:
    if json.loads(response.read()) != json.loads(manifest_bytes):
        raise SystemExit("Public channel does not match the signed release manifest")
manifest = json.loads(manifest_bytes)
with tempfile.TemporaryDirectory(prefix="jerboa-release-check-") as temp:
    root = pathlib.Path(temp)
    for component_name in ("cli", "daemon", "distro", "kernel"):
        component = manifest["components"][component_name]
        if component_name != "kernel" and component["version"] != expected:
            raise SystemExit(f"Version mismatch: {component_name}")
        assets = component.get("platforms") or component.get("files") or {"rootfs": component}
        for platform, asset in assets.items():
            target = root / (component_name + "-" + platform)
            with urlopen(asset["url"], timeout=120) as response, target.open("wb") as output:
                digest = hashlib.sha256()
                while chunk := response.read(1024 * 1024):
                    digest.update(chunk)
                    output.write(chunk)
            if digest.hexdigest() != asset["sha256"]:
                raise SystemExit(f"Public artifact checksum mismatch: {target.name}")
            if asset.get("size") and target.stat().st_size != asset["size"]:
                raise SystemExit(f"Public artifact size mismatch: {target.name}")
            if platform == "linux-amd64" and component_name in ("cli", "daemon"):
                target.chmod(0o755)
                output = subprocess.check_output([str(target), "--version"], text=True)
                if expected not in output:
                    raise SystemExit(f"Binary version mismatch: {output}")
            if component_name == "distro":
                with tarfile.open(target) as archive:
                    members = [m for m in archive if m.name.lstrip("./") == "usr/local/bin/jerboad"]
                    if len(members) != 1:
                        raise SystemExit("Distro daemon missing or duplicated")
                    binary = root / "distro-jerboad"
                    with archive.extractfile(members[0]) as source:
                        binary.write_bytes(source.read())
                    binary.chmod(0o755)
                    output = subprocess.check_output([str(binary), "--version"], text=True)
                    if expected not in output:
                        raise SystemExit(f"Distro daemon version mismatch: {output}")
            print(json.dumps({"component": component_name, "artifact": platform,
                              "version": component["version"], "result": "PASS",
                              "assertion": "downloaded bytes match signed manifest"}))
