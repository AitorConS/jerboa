#!/usr/bin/env python3
"""Simulated host power loss for Nanos TFS volumes on the macOS HVF VMM.

This is NOT a physical power-cut test. It uses a test-only VMM built with
-DHVF_CRASH_JOURNAL (firecracker-macos/experiments/hvf/durability), which
journals every guest write and every successful F_FULLFSYNC. After the VMM is
killed, crash_replay.py rebuilds the disk a power cut could leave:

  full     every write (what a VMM kill leaves; host cache survives)
  durable  only writes before the last F_FULLFSYNC (host cache fully lost)
  subset   durable plus a seeded, order-preserving subset of later writes
  boundary writes up to a sampled earlier flush plus a subset of the writes of
           the sync then in flight; checked against /data/meta, which the guest
           fsyncs only after the record it names

A guest workload acknowledges records only after fdatasync/fsync/rename/dir
fsync. For every strict round, "full" must equal the killed disk byte for byte
(journal completeness) and every model must boot and contain every
acknowledged record. Two negative controls must be detected by the "durable"
model while still passing the "full" model, proving that a plain VMM-kill test
cannot see them:
  nosync     guest skips all syncs
  flush-lie  VMM acknowledges FLUSH without a durability point

Environment:
  JERBOA_TEST_BIN   bundle bin directory (jerboa, jerboad, tools/)
  DURA_FC_JOURNAL   journal test VMM (required)
  DURA_FC_PROD      release-recipe VMM for a real-disk kill/stop regression (optional)
  DURA_GUEST        experiments/hvf/durability/guest project directory
  JERBOA_TEST_GO    Go toolchain used by the Jerboa Go driver
  DURA_ROUNDS (3), DURA_SUBSETS (3), DURA_SEED (random), DURA_EVIDENCE
"""

import hashlib
import json
import os
from pathlib import Path
import random
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
REPLAY = (ROOT / "../firecracker-macos/experiments/hvf/durability/crash_replay.py").resolve()
BIN = Path(os.environ["JERBOA_TEST_BIN"]).resolve()
FC_JOURNAL = Path(os.environ["DURA_FC_JOURNAL"]).resolve()
FC_PROD = Path(os.environ["DURA_FC_PROD"]).resolve() if os.environ.get("DURA_FC_PROD") else None
GUEST = Path(os.environ["DURA_GUEST"]).resolve()
ROUNDS = int(os.environ.get("DURA_ROUNDS", "3"))
SUBSETS = int(os.environ.get("DURA_SUBSETS", "3"))
BOUNDARIES = int(os.environ.get("DURA_BOUNDARIES", "6"))
SEED = int(os.environ.get("DURA_SEED", str(secrets.randbits(31))))
EVIDENCE_ROOT = Path(os.environ.get("DURA_EVIDENCE", ROOT / "dist/claude-followup/durability/powerloss-sim")).resolve()
WORK = Path(tempfile.mkdtemp(prefix="jdura-", dir="/tmp"))
DEST = EVIDENCE_ROOT / ("run-" + time.strftime("%Y%m%d-%H%M%S"))
JOURNAL = WORK / "journal"
RESULT = {"seed": SEED, "work": str(WORK), "rounds": [], "controls": {}, "prod": {}, "checks": []}
RNG = random.Random(SEED)
(WORK / "home").mkdir()
JOURNAL.mkdir()
TRANSCRIPT = (WORK / "commands.log").open("w")
ENV = dict(
    os.environ,
    HOME=str(WORK / "home"),
    JERBOA_HOST="unix://" + str(WORK / "daemon.sock"),
    JERBOA_AUTH_TOKEN="durability-sim-" + secrets.token_hex(8),
)


def run(args, *, success=True, env=None, timeout=600):
    args = [str(a) for a in args]
    TRANSCRIPT.write("$ " + " ".join(args) + "\n")
    result = subprocess.run(args, env=env or ENV, text=True, capture_output=True, timeout=timeout)
    TRANSCRIPT.write(result.stdout + result.stderr)
    TRANSCRIPT.flush()
    if success and result.returncode:
        raise RuntimeError(f"command failed ({result.returncode}): {' '.join(args)}\n{result.stdout}{result.stderr}")
    return result.stdout.strip()


def cli(*args, **kwargs):
    return run([BIN / "jerboa", *args], **kwargs)


def check(label):
    RESULT["checks"].append(label)
    print("PASS:", label, flush=True)


def wait_for(fn, label, timeout=180, interval=0.2):
    deadline = time.monotonic() + timeout
    last = ""
    while time.monotonic() < deadline:
        try:
            value = fn()
            if value:
                return value
        except (OSError, RuntimeError, ValueError) as error:
            last = str(error)
        time.sleep(interval)
    raise TimeoutError(f"timeout waiting for {label}: {last}")


def state(name):
    return json.loads(cli("inspect", name)).get("state")


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def max_ack(logs):
    values = [int(v) for v in re.findall(r"^ACK (\d+)\r?$", logs, re.MULTILINE)]
    return max(values) if values else 0


def acked_at_least(writer, target):
    logs = cli("logs", writer)
    if max_ack(logs) >= target:
        return True
    if state(writer) == "stopped":
        (WORK / f"{writer}-early-exit.log").write_text(logs + "\n")
        raise AssertionError(f"{writer} stopped before ACK {target}:\n{logs[-2000:]}")
    return False


class Daemon:
    process = None
    log = None

    def start(self, fc, label, **extra):
        env = dict(ENV, HVF_CRASH_JOURNAL_DIR=str(JOURNAL), **extra)
        self.log = (WORK / f"daemon-{label}.log").open("w")
        self.process = subprocess.Popen(
            [str(a) for a in (BIN / "jerboad", "--host", ENV["JERBOA_HOST"], "--tools-dir", BIN / "tools",
                              "--hypervisor", "firecracker", "--fc-bin", fc)],
            env=env, stdout=self.log, stderr=self.log,
        )
        wait_for(lambda: cli("status"), "daemon " + label)

    def stop(self):
        if self.process:
            self.process.terminate()
            try:
                self.process.wait(20)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
            self.log.close()
            self.process = None


DAEMON = Daemon()
DISK = None
FRESH = WORK / "fresh.img"


def locate_disk(name):
    for meta in (WORK / "home").rglob("*.json"):
        disk = meta.parent / "disk.img"
        if disk.exists() and f'"{name}"' in meta.read_text(errors="ignore"):
            return disk
    raise FileNotFoundError(f"disk for volume {name}")


def clone(src, dst):
    run(["cp", "-c", src, dst])


def restore_disk(image):
    # Rewrite in place: same inode and path as the daemon's volume.
    shutil.copyfile(image, DISK)


def vm_run(name, *env):
    cli("run", "dura:latest", "--name", name, "--memory", "256M", "--volume", "dvol:/data",
        *[a for pair in env for a in ("-e", pair)])


def vm_remove(name, force=False):
    try:
        if state(name) != "stopped":
            cli("stop", name, *(["--force"] if force else []))
            wait_for(lambda: state(name) == "stopped", name + " stopped", timeout=120)
    finally:
        cli("rm", name, success=False)


def verify(label, acked):
    name = f"dura-v-{label}"
    vm_run(name, "DURA_MODE=verify", f"DURA_ACK={acked}")
    try:
        wait_for(lambda: state(name) == "stopped", name + " stopped", timeout=240)
        outcome = "exited"
    except TimeoutError:
        outcome = "timeout"
    logs = cli("logs", name)
    (WORK / f"{name}.log").write_text(logs + "\n")
    vm_remove(name, force=True)
    found = re.search(r"VERIFY_OK acked=(\d+) present=(\d+) meta=(\d+)", logs)
    failure = re.search(r"DURA_FAIL (.*)", logs)
    return {
        "ok": bool(found),
        "outcome": outcome,
        "present": int(found.group(2)) if found else None,
        "meta": int(found.group(3)) if found else None,
        "failure": failure.group(1).strip() if failure else (None if found else "no VERIFY_OK in log"),
    }


def replay(round_dir, inode, mode, seed=0, compare=None, flush=0):
    out = round_dir / f"{mode}-{seed}.img"
    summary = json.loads(run([sys.executable, REPLAY, "--base", round_dir / "base.img", "--journals", round_dir / "journal",
                              "--inode", inode, "--mode", mode, "--seed", seed, "--flush", flush, "--out", out,
                              *(["--compare", compare] if compare else [])]))
    return out, summary


def crash_round(label, *, sync=True, models=("durable",)):
    round_dir = WORK / label
    (round_dir / "journal").mkdir(parents=True)
    restore_disk(FRESH)
    clone(DISK, round_dir / "base.img")
    inode = DISK.stat().st_ino
    for stale in JOURNAL.glob("journal-*.bin"):
        stale.unlink()
    target = RNG.randint(60, 300)
    writer = f"dura-w-{label}"
    vm_run(writer, "DURA_MODE=write", *([] if sync else ["DURA_SYNC=0"]))
    wait_for(lambda: acked_at_least(writer, target), f"{writer} ACK {target}", timeout=300)
    acked = max_ack(cli("logs", writer))
    cli("stop", writer, "--force")
    wait_for(lambda: state(writer) == "stopped", writer + " stopped", timeout=120)
    logs = cli("logs", writer)
    (WORK / f"{writer}.log").write_text(logs + "\n")
    cli("rm", writer)
    for journal in JOURNAL.glob("journal-*.bin"):
        shutil.move(journal, round_dir / "journal" / journal.name)
    clone(DISK, round_dir / "killed.img")
    killed_sha = sha256(round_dir / "killed.img")
    full, full_summary = replay(round_dir, inode, "full", compare=round_dir / "killed.img")
    result = {
        "label": label, "sync": sync, "target_ack": target, "acked_before_kill": acked,
        "last_ack_in_log": max_ack(logs), "killed_sha256": killed_sha,
        "full_sha256": sha256(full), "journal": full_summary, "models": {},
    }
    result["journal_complete"] = full_summary["compare"]["complete"]
    for mode, seed in [("full", 0)] + [(m, s) for m in models if m != "boundary" for s in (range(1, SUBSETS + 1) if m == "subset" else [0])]:
        image, summary = (full, full_summary) if mode == "full" else replay(round_dir, inode, mode, seed)
        restore_disk(image)
        outcome = verify(f"{label}-{mode}-{seed}", acked)
        outcome["replay"] = summary
        result["models"][f"{mode}-{seed}"] = outcome
        if mode != "full":
            image.unlink()
    if "boundary" in models and full_summary["write_bearing_flushes"]:
        candidates = full_summary["write_bearing_flushes"]
        points = sorted(RNG.sample(candidates, min(BOUNDARIES, len(candidates))))
        for point in points:
            image, summary = replay(round_dir, inode, "boundary", seed=point, flush=point)
            restore_disk(image)
            outcome = verify(f"{label}-boundary-{point}", -1)
            outcome["replay"] = summary
            result["models"][f"boundary-{point}"] = outcome
            image.unlink()
    # Keep the images for diagnosis when the journal did not match the killed disk.
    if result["journal_complete"]:
        for img in round_dir.glob("*.img"):
            img.unlink()
    return result


def retain():
    DEST.mkdir(parents=True, exist_ok=True)
    shutil.copytree(WORK, DEST, dirs_exist_ok=True,
                    ignore=shutil.ignore_patterns("home", "*.img", "journal", "journal-*.bin", "*.sock"))
    (DEST / "RESULT.json").write_text(json.dumps(RESULT, indent=2) + "\n")


def main():
    global DISK
    hashed = (BIN / "jerboa", BIN / "jerboad", BIN / "tools/kernel.img", FC_JOURNAL, REPLAY,
              GUEST / "main.go", GUEST / "unikernel.toml")
    for required in hashed:
        if not required.exists():
            raise FileNotFoundError(required)
    RESULT["inputs"] = {str(p): sha256(p) for p in hashed}
    if FC_PROD:
        RESULT["inputs"][str(FC_PROD)] = sha256(FC_PROD)
    print("Work:", WORK, "evidence:", DEST, "seed:", SEED, flush=True)
    try:
        DAEMON.start(FC_JOURNAL, "journal")
        project = WORK / "guest-project"
        shutil.copytree(GUEST, project)
        go_path = str(Path(os.environ["JERBOA_TEST_GO"]).resolve().parent)
        cli("build", project, "--platform", "linux/arm64", "--name", "dura",
            env=dict(ENV, PATH=go_path + os.pathsep + ENV["PATH"]))
        cli("volume", "create", "dvol", "--size", "64M")
        DISK = locate_disk("dvol")
        # jerboad formats an unformatted volume on the host at its first run
        # (volume.EnsureFormatted: TFS header at sector 0, file grows). That write
        # is outside the VMM journal, so the baseline must be taken after it.
        formatted = verify("format", 0)
        assert formatted["ok"], formatted
        with open(DISK, "rb") as disk:
            assert disk.read(6) == b"NVMTFS", "volume was not formatted by the first run"
        clone(DISK, FRESH)
        RESULT["fresh_volume"] = {"bytes": FRESH.stat().st_size, "sha256": sha256(FRESH)}

        for number in range(1, ROUNDS + 1):
            r = crash_round(f"round{number}", models=("durable", "subset", "boundary"))
            RESULT["rounds"].append(r)
            assert r["journal_complete"], f"{r['label']}: replayed full image differs from killed disk"
            assert r["journal"]["flushes"] > 0, r
            boundaries = 0
            for model, outcome in r["models"].items():
                assert outcome["ok"], f"{r['label']} {model}: {outcome}"
                if model.startswith("boundary-"):
                    boundaries += 1
                    assert outcome["present"] >= outcome["meta"], f"{r['label']} {model}: {outcome}"
                else:
                    assert outcome["present"] >= r["acked_before_kill"], f"{r['label']} {model}: {outcome}"
            assert boundaries > 0, r
            check(f"{r['label']}: journal complete; {r['acked_before_kill']} acknowledged records survive "
                  f"full, durable and {SUBSETS} partial-writeback models; {boundaries} earlier flush-boundary "
                  "cuts keep every record named by fsynced metadata")

        # Without syncs Nanos may keep everything in its own page cache, so a plain
        # VMM kill can already lose it; the full-model outcome is recorded only.
        # flush-lie below is the control that a VMM kill cannot see.
        r = crash_round("nosync", sync=False)
        RESULT["controls"]["nosync"] = r
        assert r["journal_complete"], r
        assert not r["models"]["durable-0"]["ok"], f"nosync durable model did not detect loss: {r}"
        check("negative control nosync: loss detected by durable model (full model: "
              f"{'recovered' if r['models']['full-0']['ok'] else 'also lost'}, "
              f"{r['journal']['writes']} guest writes reached the VMM)")

        DAEMON.stop()
        DAEMON.start(FC_JOURNAL, "flush-lie", HVF_CRASH_JOURNAL_LIE="1")
        r = crash_round("flushlie")
        RESULT["controls"]["flush_lie"] = r
        assert r["journal_complete"], r
        assert r["journal"]["flushes"] == 0, r
        assert r["models"]["full-0"]["ok"], f"flush-lie full model should pass: {r}"
        assert not r["models"]["durable-0"]["ok"], f"flush-lie durable model did not detect loss: {r}"
        check("negative control flush-lie: invisible to VMM kill, detected by durable model")

        if FC_PROD:
            DAEMON.stop()
            DAEMON.start(FC_PROD, "prod")
            restore_disk(FRESH)
            writer = "dura-w-prod"
            vm_run(writer, "DURA_MODE=write")
            wait_for(lambda: acked_at_least(writer, 150), "prod ACK 150", timeout=300)
            acked = max_ack(cli("logs", writer))
            cli("stop", writer, "--force")
            wait_for(lambda: state(writer) == "stopped", "prod writer stopped", timeout=120)
            (WORK / f"{writer}.log").write_text(cli("logs", writer) + "\n")
            cli("rm", writer)
            outcome = verify("prod-kill", acked)
            RESULT["prod"]["kill"] = dict(outcome, acked_before_kill=acked)
            assert outcome["ok"] and outcome["present"] >= acked, outcome
            check(f"release-recipe VMM: {acked} acknowledged records survive a real VMM kill on the host file")

            restore_disk(FRESH)
            writer = "dura-w-prod-limit"
            vm_run(writer, "DURA_MODE=write", "DURA_LIMIT=200")
            wait_for(lambda: state(writer) == "stopped", "prod limited writer exit", timeout=300)
            logs = cli("logs", writer)
            (WORK / f"{writer}.log").write_text(logs + "\n")
            cli("rm", writer)
            assert "WRITE_DONE" in logs and "DURA_FAIL" not in logs, logs
            outcome = verify("prod-exit", 200)
            RESULT["prod"]["guest_exit"] = outcome
            assert outcome["ok"] and outcome["present"] == 200, outcome
            check("release-recipe VMM: guest exit after 200 records, data verified")

        daemon_logs = "".join(p.read_text(errors="ignore") for p in WORK.glob("daemon-*.log"))
        RESULT["host_storage_errors_logged"] = daemon_logs.count("host storage error")
        RESULT["status"] = "PASS"
        print("PASS: simulated power-loss durability", flush=True)
    except Exception as error:
        RESULT["status"] = "FAIL"
        RESULT["error"] = str(error)
        raise
    finally:
        try:
            for vm in json.loads(cli("ps", "--output", "json") or "[]"):
                vm_remove(vm["name"], force=True)
        except Exception:
            pass
        try:
            cli("volume", "rm", "dvol", success=False)
        except Exception:
            pass
        DAEMON.stop()
        TRANSCRIPT.close()
        retain()


if __name__ == "__main__":
    main()
