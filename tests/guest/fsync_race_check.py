#!/usr/bin/env python3
"""Check that a concurrent fsync never completes before its metadata is
written (tests/guest/fsync_race.c).

Needs the macOS test VMM built with -DHVF_CRASH_JOURNAL. Journal sequence
numbers are global across drives, so the root disk rebuilt from the writes
issued before each /data progress block shows what the guest had written when
that round's fsyncs had all returned: every file of the round must be present
and intact. The `dump` tool reads the rebuilt images. Finally the killed disk
is booted again and must verify every acknowledged file.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import time

THREADS, ROUNDS, BLOCK = 4, 200, 4096


def boot(a, config, console, journal_dir, until, timeout):
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


def expected(t, r):
    b = bytearray([ord("a") + t]) * BLOCK
    tag = f"RACE-{t}-{r:05d}".encode()
    b[:len(tag)] = tag
    b[len(tag)] = 0
    return bytes(b)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ("kernel", "mkfs", "dump", "firecracker", "program", "work", "replay"):
        p.add_argument("--" + name, type=Path, required=True)
    a = p.parse_args()
    a.work.mkdir(parents=True)
    work = a.work.resolve()
    spec = importlib.util.spec_from_file_location("crash_replay", a.replay)
    replay = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(replay)
    root, volume = work / "root.img", work / "volume.img"
    manifest = (f"(children:( data:(children:()) program:(contents:(host:{json.dumps(str(a.program.resolve()))}))) "
                f"program:/program arguments:(0:\"/program\") environment:())")
    subprocess.run([str(a.mkfs), "-c", "-s", "64M", str(root)], input=manifest, text=True,
                   check=True, capture_output=True)
    subprocess.run([str(a.mkfs), "-e", "-l", "testdata", "-s", "16M", str(volume)], check=True, capture_output=True)
    base = work / "root-base.img"
    shutil.copyfile(root, base)
    (work / "mounts").write_text("testdata:/data")
    config = work / "config.json"
    config.write_text(json.dumps({
        "boot-source": {"kernel_image_path": str(a.kernel.resolve()), "boot_protocol": "elf"},
        "machine-config": {"vcpu_count": 4, "mem_size_mib": 256, "power_button": True},
        "drives": [{"drive_id": "rootfs", "path_on_host": str(root), "is_root_device": True, "is_read_only": False},
                   {"drive_id": "data", "path_on_host": str(volume), "is_root_device": False, "is_read_only": False}],
        "firmware": {"opt/uni/mounts": str(work / "mounts")},
        "security": {"mode": "hardened", "version": 1},
    }))
    journal = work / "journal"
    journal.mkdir()
    out = boot(a, config, work / "write.log", journal, "RACE CRASH POINT", 300)
    if "RACE CRASH POINT" not in out:
        raise SystemExit(f"FAIL: workload did not finish; see {work / 'write.log'}")

    records = []
    for j in sorted(journal.glob("journal-*.bin")):
        records += replay.parse(j)[0]
    rino, vino = root.stat().st_ino, volume.stat().st_ino
    applied = {r["seq"]: r["len"] for r in records if r["type"] == "S"}
    writes = [r for r in records if r["type"] == "W" and r["ino"] in (rino, vino)]
    for w in writes:
        w["data"] = w["data"][:applied.get(w["seq"], len(w["data"]))]
    progress = []
    for w in writes:
        if w["ino"] == vino:
            for m in re.finditer(rb"PROGRESS-(\d{5})", w["data"]):
                progress.append((w["seq"], int(m.group(1))))
    if len(progress) != ROUNDS:
        raise SystemExit(f"FAIL: found {len(progress)} progress writes, expected {ROUNDS}")

    image = work / "root-at.img"
    shutil.copyfile(base, image)
    pending = [w for w in writes if w["ino"] == rino]
    late = []
    with image.open("r+b") as f:
        i = 0
        for seq, rnd in progress:
            while i < len(pending) and pending[i]["seq"] < seq:
                f.seek(pending[i]["offset"])
                f.write(pending[i]["data"])
                i += 1
            f.flush()
            target = work / "dump"
            shutil.rmtree(target, ignore_errors=True)
            subprocess.run([str(a.dump), "-d", str(target), str(image)], check=True, capture_output=True)
            for t in range(THREADS):
                for r in range(rnd + 1):
                    path = target / "r" / f"{t}-{r:05d}"
                    if not path.exists() or path.read_bytes() != expected(t, r):
                        got = path.read_bytes() if path.exists() else b""
                        late.append({"round": rnd, "file": f"{t}-{r:05d}",
                                     "state": "missing" if not path.exists() else
                                     f"length {len(got)}" if len(got) != BLOCK else
                                     "zero" if not any(got) else "wrong data"})
        while i < len(pending):
            f.seek(pending[i]["offset"])
            f.write(pending[i]["data"])
            i += 1
    complete = image.read_bytes() == root.read_bytes()
    shutil.rmtree(work / "dump", ignore_errors=True)
    image.unlink()

    vjournal = work / "journal-verify"
    vjournal.mkdir()
    out = boot(a, config, work / "verify.log", vjournal, None, 120)
    verified = "RACE VERIFY PASS" in out
    result = {"writes": len(writes), "progress": len(progress), "journal_complete": complete,
              "late": len(late), "late_samples": late[:20], "restart_verified": verified}
    (work / "RESULT.json").write_text(json.dumps(result, indent=1) + "\n")
    for img in (root, volume, base):
        img.unlink()
    shutil.rmtree(journal)
    shutil.rmtree(vjournal)
    print(json.dumps({k: v for k, v in result.items() if k != "late_samples"}))
    for s in late[:5]:
        print("late:", s)
    if not complete:
        raise SystemExit("FAIL: journal does not reproduce the killed root disk")
    if late or not verified:
        raise SystemExit("FAIL")
    print("FSYNC RACE PASS")


if __name__ == "__main__":
    main()
