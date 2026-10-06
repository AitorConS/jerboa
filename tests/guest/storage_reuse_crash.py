#!/usr/bin/env python3
"""Check that storage released by truncate/unlink is not reused before the
metadata releasing it is durable (tests/guest/storage_reuse.c).

Needs the macOS test VMM built with -DHVF_CRASH_JOURNAL (see
firecracker-macos/experiments/hvf/durability). The first boot writes and is
killed at REUSE CRASH POINT. From the journal of the /data volume, every cut a
host power loss could leave is rebuilt: the writes before each successful
F_FULLFSYNC, plus the following interval's writes with any single write
dropped, plus all of them. Each distinct image is booted and verified.

The run fails if the journal does not reproduce the killed disk, if b never
reused storage released by the a files (the test would prove nothing), or if
any cut shows another file's data (LEAK) or other damage.
"""
import argparse
import hashlib
import importlib.util
import json
import os
import re
from pathlib import Path
import shutil
import signal
import subprocess
import time

BLOCK = 4096


def load_replay(path):
    spec = importlib.util.spec_from_file_location("crash_replay", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def boot(a, config, console, journal_dir, until, timeout):
    """Run the VMM until it exits, or kill it once `until` appears."""
    env = dict(os.environ, HVF_CRASH_JOURNAL_DIR=str(journal_dir))
    with console.open("w") as log:
        p = subprocess.Popen([str(a.firecracker), "--no-api", "--config-file", str(config)],
                             stdin=subprocess.DEVNULL, stdout=log, stderr=log, env=env)
        deadline = time.monotonic() + timeout
        while p.poll() is None and time.monotonic() < deadline:
            if until and until in console.read_text(errors="replace"):
                p.send_signal(signal.SIGKILL)
                break
            time.sleep(0.2)
        if p.poll() is None:
            p.kill()
        p.wait()
    return console.read_text(errors="replace")


def tagged_blocks(data, offset, tag):
    # File blocks need not be 4 KiB aligned on the volume: scan every sector.
    found = set()
    start = (-offset) % 512
    for i in range(start, len(data) - BLOCK + 1, 512):
        if data[i:i + 6] == b"JRUSE-" and data[i + 6:i + 7] in tag:
            found.add(offset + i)
    return found


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ("kernel", "mkfs", "firecracker", "program", "work", "replay"):
        p.add_argument("--" + name, type=Path, required=True)
    p.add_argument("--volume-size", default="64M")
    p.add_argument("--max-cuts", type=int, default=400)
    p.add_argument("--fail-on", default="LEAK,FOREIGN,CORRUPT,ZERO",
                   help="failure kinds that fail the run (others are only reported)")
    a = p.parse_args()
    a.work.mkdir(parents=True)
    work = a.work.resolve()
    replay = load_replay(a.replay)
    root, volume = work / "root.img", work / "volume.img"
    manifest = (f"(children:( data:(children:()) program:(contents:(host:{json.dumps(str(a.program.resolve()))}))) "
                f"program:/program arguments:(0:\"/program\") environment:())")
    subprocess.run([str(a.mkfs), "-c", "-s", "512M", str(root)], input=manifest, text=True,
                   check=True, capture_output=True)
    subprocess.run([str(a.mkfs), "-e", "-l", "testdata", "-s", a.volume_size, str(volume)],
                   check=True, capture_output=True)
    base = work / "base.img"
    shutil.copyfile(volume, base)
    (work / "mounts").write_text("testdata:/data")
    config = work / "config.json"
    config.write_text(json.dumps({
        "boot-source": {"kernel_image_path": str(a.kernel.resolve()), "boot_protocol": "elf"},
        "machine-config": {"vcpu_count": 2, "mem_size_mib": 256, "power_button": True},
        "drives": [{"drive_id": "rootfs", "path_on_host": str(root), "is_root_device": True, "is_read_only": False},
                   {"drive_id": "data", "path_on_host": str(volume), "is_root_device": False, "is_read_only": False}],
        "firmware": {"opt/uni/mounts": str(work / "mounts")},
        "security": {"mode": "hardened", "version": 1},
    }))
    result = {"kernel_sha256": hashlib.sha256(a.kernel.read_bytes()).hexdigest(), "cuts": []}

    journal = work / "journal"
    journal.mkdir()
    out = boot(a, config, work / "write.log", journal, "REUSE CRASH POINT", 300)
    if "REUSE CRASH POINT" not in out:
        raise SystemExit(f"FAIL: write phase did not reach the crash point; see {work / 'write.log'}")
    killed = work / "killed.img"
    shutil.copyfile(volume, killed)

    inode = volume.stat().st_ino
    records = []
    for j in sorted(journal.glob("journal-*.bin")):
        recs, _ = replay.parse(j)
        records += [r for r in recs if r["ino"] == inode and r["type"] in "WSF"]
    applied = {r["seq"]: r["len"] for r in records if r["type"] == "S"}
    writes = [r for r in records if r["type"] == "W"]
    for w in writes:
        w["data"] = w["data"][:applied.get(w["seq"], len(w["data"]))]
    flushes = [r["seq"] for r in records if r["type"] == "F"]

    def build(chosen, path):
        shutil.copyfile(base, path)
        with path.open("r+b") as image:
            for w in chosen:
                image.seek(w["offset"])
                image.write(w["data"])

    full = work / "full.img"
    build(writes, full)
    complete = full.read_bytes() == killed.read_bytes()
    result.update(writes=len(writes), flushes=len(flushes), journal_complete=complete)
    full.unlink()
    if not complete:
        raise SystemExit("FAIL: journal does not reproduce the killed volume")

    a_blocks, b_blocks = set(), set()
    for w in writes:
        a_blocks |= tagged_blocks(w["data"], w["offset"], (b"1", b"2", b"3"))
        b_blocks |= tagged_blocks(w["data"], w["offset"], (b"B",))
    reused = len(a_blocks & b_blocks)
    result["reused_blocks"] = reused
    print(f"journal: {len(writes)} writes, {len(flushes)} flushes, b reused {reused} blocks of a1..a3")
    if not reused:
        raise SystemExit("FAIL: b did not reuse released storage; the test proves nothing")

    # Cuts: everything before flush k, plus the next interval minus at most one write.
    cuts, seen = [], set()
    bounds = flushes + [float("inf")]
    for k, point in enumerate(flushes):
        before = [w for w in writes if w["seq"] < point]
        interval = [w for w in writes if point < w["seq"] < bounds[k + 1]]
        if not interval:
            continue
        variants = [interval] + [interval[:i] + interval[i + 1:] for i in range(len(interval))]
        for v in variants:
            key = tuple(w["seq"] for w in before + v)
            if key not in seen:
                seen.add(key)
                cuts.append((k + 1, len(interval) - len(v), before + v))
    if len(cuts) > a.max_cuts:
        raise SystemExit(f"FAIL: {len(cuts)} cuts exceed --max-cuts")
    print(f"verifying {len(cuts)} distinct cuts")
    failures = 0
    fail_on = set(a.fail_on.split(",")) | {"NOVERIFY"}
    vjournal = work / "journal-verify"
    for n, (flush, dropped, chosen) in enumerate(cuts):
        build(chosen, volume)
        vjournal.mkdir(exist_ok=True)
        log = work / f"verify-{n:03d}.log"
        out = boot(a, config, log, vjournal, None, 120)
        shutil.rmtree(vjournal)
        ok = "REUSE VERIFY PASS" in out
        leak = "REUSE FAIL LEAK" in out
        kinds = sorted(set(re.findall(r"^REUSE KIND (\w+)", out, re.MULTILINE)))
        if not ok and not kinds:
            kinds = ["NOVERIFY"]
        cut = {"cut": n, "flush": flush, "dropped": dropped, "writes": len(chosen),
               "ok": ok, "leak": leak, "kinds": kinds,
               "lines": [l for l in out.splitlines() if l.startswith("REUSE")]}
        result["cuts"].append(cut)
        if ok:
            log.unlink()
        elif not set(kinds) & fail_on:
            print(f"cut {n} (flush {flush}, dropped {dropped}): reported only: {kinds}")
        else:
            failures += 1
            print(f"cut {n} (flush {flush}, dropped {dropped}): " + "; ".join(cut["lines"][:6]))
    result["failures"] = failures
    result["leaks"] = sum(c["leak"] for c in result["cuts"])
    result["kinds"] = {k: sum(k in c["kinds"] for c in result["cuts"])
                       for k in sorted({k for c in result["cuts"] for k in c["kinds"]})}
    print("failure kinds by cut:", result["kinds"])
    (work / "RESULT.json").write_text(json.dumps(result, indent=1) + "\n")
    for img in (volume, killed, base, root):
        img.unlink()
    shutil.rmtree(journal)
    if failures:
        raise SystemExit(f"FAIL: {failures} of {len(cuts)} cuts ({result['leaks']} leaks)")
    print(f"STORAGE REUSE CRASH PASS: {len(cuts)} cuts")


if __name__ == "__main__":
    main()
