#!/usr/bin/env python3
"""Boot a static regression binary with a supplied Jerboa toolchain.

No daemon, installed images, or persistent volumes are used. Keep --work to
inspect the raw disk and log. Reusing it with --reuse-disk tests persistence.
"""
import argparse
import json
from pathlib import Path
import platform
import subprocess


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ("kernel", "mkfs", "firecracker", "program", "work"):
        p.add_argument("--" + name, type=Path, required=True)
    p.add_argument("--cpus", type=int, default=4)
    p.add_argument("--memory", type=int, default=256)
    p.add_argument("--disk-size", default="2G")
    p.add_argument("--timeout", type=int, default=180)
    p.add_argument("--expect", default="BENCHMARK REGRESSIONS PASS")
    p.add_argument("--reuse-disk", action="store_true")
    p.add_argument("args", nargs="*")
    a = p.parse_args()
    a.work.mkdir(parents=True, exist_ok=True)
    disk = (a.work / "root.img").resolve()
    if not a.reuse_disk:
        if disk.exists():
            p.error("work directory already contains root.img; use another directory or --reuse-disk")
        # JSON quoting is also valid for manifest strings used here.
        arguments = " ".join(f"{i}:{json.dumps(v)}" for i, v in enumerate(["/program", *a.args]))
        manifest = (f"(children:(program:(contents:(host:{json.dumps(str(a.program.resolve()))}))) "
                    f"program:/program arguments:({arguments}) environment:())")
        subprocess.run([str(a.mkfs.resolve()), "-c", "-s", a.disk_size, str(disk)],
                       input=manifest, text=True, check=True, capture_output=True)
    config = {
        "boot-source": {"kernel_image_path": str(a.kernel.resolve())},
        "machine-config": {"vcpu_count": a.cpus, "mem_size_mib": a.memory},
        "drives": [{"drive_id": "rootfs", "path_on_host": str(disk),
                    "is_root_device": True, "is_read_only": False}],
    }
    if platform.system() == "Darwin":
        config["boot-source"]["boot_protocol"] = "elf"
        config["machine-config"]["power_button"] = True
        config["security"] = {"mode": "hardened", "version": 1}
    else:
        config["boot-source"]["boot_args"] = "console=ttyS0 reboot=k panic=1 pci=off"
    config_path = a.work / "config.json"
    config_path.write_text(json.dumps(config))
    with (a.work / "console.log").open("w") as log:
        try:
            result = subprocess.run([str(a.firecracker.resolve()), "--no-api", "--config-file",
                                     str(config_path.resolve())], stdin=subprocess.DEVNULL,
                                     stdout=log, stderr=log, timeout=a.timeout)
        except subprocess.TimeoutExpired:
            raise SystemExit(f"FAIL: guest timed out; see {a.work / 'console.log'}")
    output = (a.work / "console.log").read_text(errors="replace")
    print(output, end="")
    if result.returncode or a.expect not in output:
        raise SystemExit(f"FAIL: exit={result.returncode}, required marker={a.expect!r}")


if __name__ == "__main__":
    main()
